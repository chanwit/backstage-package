/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/go-git/go-git/v5/plumbing"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/go-git/go-git/v5"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	backstagev1alpha1 "backstage-package/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = backstagev1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {

	app := &cli.App{
		Name:  "backstage-package",
		Usage: "A tool for Backstage application composition",
		Action: func(c *cli.Context) error {
			return nil
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "filename",
				Aliases: []string{"f"},
				Usage:   "Specify the profile configuration file, - for stdin",
				Value:   "application.yaml",
			},
		},
		Commands: []*cli.Command{
			{
				Name:    "generate",
				Aliases: []string{"g"},
				Usage:   "generate application and deployment manifests",
				Action:  generateAction,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "outdir",
						Aliases: []string{"d"},
						Usage:   "Specify output directory for deployment manifests",
						Value:   "dist/",
					},
				},
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}

}

func generateAction(c *cli.Context) error {
	var (
		content []byte
		err     error
	)

	filename := c.String("filename")
	if filename == "-" {
		content, err = ioutil.ReadAll(os.Stdin)
	} else {
		content, err = ioutil.ReadFile(filename)
		if err != nil {
			return err
		}
	}

	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(content, nil, nil)
	if err != nil {
		return err
	}

	application, ok := obj.(*backstagev1alpha1.Application)
	if !ok {
		return errors.New("Expecting application manifest")
	}

	dir, err := ioutil.TempDir("/tmp", "staging-")
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "backstage")
	defer os.RemoveAll(dir) // clean up

	fmt.Printf("Staging Dir: %s\n", dir)

	spec := application.Spec

	var ref plumbing.ReferenceName
	rev := spec.TemplateRepository.Revision
	if rev.Branch != "" {
		ref = plumbing.NewBranchReferenceName(rev.Branch)
	} else if rev.Tag != "" {
		ref = plumbing.NewTagReferenceName(rev.Tag)
	} else if rev.Commit != "" {
		ref = plumbing.ReferenceName(rev.Commit)
	}

	fmt.Printf("Clone app template from %s\n", spec.TemplateRepository.URL)

	r, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL:           spec.TemplateRepository.URL,
		Depth:         1,
		ReferenceName: ref,
		SingleBranch:  true,
		Progress:      os.Stdout,
		NoCheckout:    false,
		Tags:          git.AllTags,
	})
	if err != nil {
		return err
	}

	i, err := r.Log(&git.LogOptions{})
	if err != nil {
		return err
	}
	defer i.Close()

	commit, err := i.Next()
	if err != nil {
		return err
	}

	fmt.Println("Showing latest commit ...")
	fmt.Println(commit.String())

	tmpl, err := template.New("root_tsx").Parse(rootTsx)
	if err != nil {
		return err
	}

	out := &bytes.Buffer{}
	err = tmpl.Execute(out, map[string]string{
		"imports": spec.RootComponent.Imports,
		"sidebar": spec.RootComponent.Sidebar,
	})
	if err != nil {
		return err
	}

	rootTsxOut := filepath.Join(dir, spec.RootComponent.Path)
	ioutil.WriteFile(rootTsxOut, out.Bytes(), 0644)

	// read app package.json
	packageJsonFilename := filepath.Join(dir, "packages/app/package.json")
	packageJsonBytes, err := ioutil.ReadFile(packageJsonFilename)
	if err != nil {
		return err
	}

	packageJson := map[string]interface{}{}
	json.Unmarshal(packageJsonBytes, &packageJson)
	if err != nil {
		return err
	}

	dependencies := packageJson["dependencies"].(map[string]interface{})
	newDep := map[string]interface{}{}
	for k, v := range dependencies {
		if strings.HasPrefix(k, "@backstage/plugin-") {
			continue
		}
		newDep[k] = v
	}

	// add plugins to newDep
	for _, p := range spec.Plugins {
		newDep[p.Package] = p.Version
	}

	packageJson["dependencies"] = newDep
	packageJsonOut, err := json.Marshal(packageJson)
	if err != nil {
		return err
	}
	ioutil.WriteFile(packageJsonFilename, packageJsonOut, 0644)

	pluginTsOut := bytes.NewBufferString(`
/*
 * Copyright 2020 Spotify AB
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
`)
	for _, p := range spec.Plugins {
		pluginTsOut.WriteString(fmt.Sprintf("export { plugin as %s } from '%s';\n", p.Name, p.Package))
	}

	pluginTsFilename := filepath.Join(dir, "packages/app/src/plugins.ts")
	ioutil.WriteFile(pluginTsFilename, pluginTsOut.Bytes(), 0644)

	if err := yarnInstall(dir); err != nil {
		return err
	}

	if err := yarnBundle(dir); err != nil {
		return err
	}

	imageName := spec.ContainerImageRepositoryPrefix + "backstage:latest"
	if err := dockerBuildImage(imageName, dir); err != nil {
		return err
	}

	if err := dockerPushImage(imageName, dir); err != nil {
		return err
	}

	outdir := c.String("outdir")
	os.Mkdir(outdir, 0755)

	for _, target := range spec.DeploymentTargets {
		fmt.Printf("Checking deployment target: %s\n", target)
		if target == backstagev1alpha1.Kubernetes {
			fmt.Printf("Found deployment target: %s\n", target)
			if err := generateKubernetesManifests(outdir, imageName); err != nil {
				return err
			}
		}
	}

	return nil
}

func yarnInstall(dir string) error {
	if err := run(dir, "yarn", "install"); err != nil {
		return errors.Errorf("error running yarn install: %v", err)
	}
	return nil
}

func yarnBundle(dir string) error {
	if err := run(dir, "yarn", "bundle"); err != nil {
		return errors.Errorf("error running yarn bundle: %v", err)
	}
	return nil
}

func dockerBuildImage(imageName string, dir string) error {
	if err := run(dir, "docker", "build", "-t", imageName, "."); err != nil {
		return errors.Errorf("error running docker build: %v", err)
	}
	return nil
}

func dockerPushImage(imageName string, dir string) error {
	if err := run(dir, "docker", "push", imageName); err != nil {
		return errors.Errorf("error running docker push: %v", err)
	}
	return nil
}

func run(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func generateKubernetesManifests(outdir string, imageName string) error {
	fmt.Println("Generating manifests ...")
	deploymentFilename := filepath.Join(outdir, "app_manifests.yaml")
	deployment := fmt.Sprintf(`
apiVersion: extensions/v1beta1
kind: Deployment
metadata:
  labels:
    app: backstage
  name: backstage
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: backstage
  template:
    metadata:
      labels:
        app: backstage
    spec:
      containers:
      - image: %s
        imagePullPolicy: Always
        name: backstage
---
apiVersion: v1
kind: Service
metadata:
  labels:
    app: backstage
  name: backstage
  namespace: default
spec:
  ports:
  - port: 80
    protocol: TCP
    targetPort: 80
  selector:
    app: backstage
  type: NodePort
`, imageName)

	err := ioutil.WriteFile(deploymentFilename, []byte(deployment), 0644)
	if err != nil {
		return err
	}

	return nil
}
