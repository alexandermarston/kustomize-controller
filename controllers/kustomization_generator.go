/*
Copyright 2020 The Flux authors

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

package controllers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"sigs.k8s.io/kustomize/api/filesys"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/provider"
	"sigs.k8s.io/kustomize/api/resmap"
	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"

	"github.com/fluxcd/pkg/apis/kustomize"
	securefs "github.com/fluxcd/pkg/kustomize/filesys"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
)

type KustomizeGenerator struct {
	root          string
	kustomization kustomizev1.Kustomization
}

func NewGenerator(root string, kustomization kustomizev1.Kustomization) *KustomizeGenerator {
	return &KustomizeGenerator{
		root:          root,
		kustomization: kustomization,
	}
}

func (kg *KustomizeGenerator) WriteFile(dirPath string) error {
	if err := kg.generateKustomization(dirPath); err != nil {
		return err
	}

	kfile := filepath.Join(dirPath, konfig.DefaultKustomizationFileName())

	data, err := os.ReadFile(kfile)
	if err != nil {
		return err
	}

	kus := kustypes.Kustomization{
		TypeMeta: kustypes.TypeMeta{
			APIVersion: kustypes.KustomizationVersion,
			Kind:       kustypes.KustomizationKind,
		},
	}

	if err := yaml.Unmarshal(data, &kus); err != nil {
		return err
	}

	if kg.kustomization.Spec.TargetNamespace != "" {
		kus.Namespace = kg.kustomization.Spec.TargetNamespace
	}

	for _, m := range kg.kustomization.Spec.Patches {
		kus.Patches = append(kus.Patches, kustypes.Patch{
			Patch:  m.Patch,
			Target: adaptSelector(&m.Target),
		})
	}

	for _, m := range kg.kustomization.Spec.PatchesStrategicMerge {
		kus.PatchesStrategicMerge = append(kus.PatchesStrategicMerge, kustypes.PatchStrategicMerge(m.Raw))
	}

	for _, m := range kg.kustomization.Spec.PatchesJSON6902 {
		patch, err := json.Marshal(m.Patch)
		if err != nil {
			return err
		}
		kus.PatchesJson6902 = append(kus.PatchesJson6902, kustypes.Patch{
			Patch:  string(patch),
			Target: adaptSelector(&m.Target),
		})
	}

	for _, image := range kg.kustomization.Spec.Images {
		newImage := kustypes.Image{
			Name:    image.Name,
			NewName: image.NewName,
			NewTag:  image.NewTag,
			Digest:  image.Digest,
		}
		if exists, index := checkKustomizeImageExists(kus.Images, image.Name); exists {
			kus.Images[index] = newImage
		} else {
			kus.Images = append(kus.Images, newImage)
		}
	}

	kd, err := yaml.Marshal(kus)
	if err != nil {
		return err
	}
	return os.WriteFile(kfile, kd, os.ModePerm)
}

func checkKustomizeImageExists(images []kustypes.Image, imageName string) (bool, int) {
	for i, image := range images {
		if imageName == image.Name {
			return true, i
		}
	}

	return false, -1
}

func (kg *KustomizeGenerator) generateKustomization(dirPath string) error {
	fs, err := securefs.MakeFsOnDiskSecure(kg.root)
	if err != nil {
		return err
	}

	// Determine if there already is a Kustomization file at the root,
	// as this means we do not have to generate one.
	for _, kfilename := range konfig.RecognizedKustomizationFileNames() {
		if kpath := filepath.Join(dirPath, kfilename); fs.Exists(kpath) && !fs.IsDir(kpath) {
			return nil
		}
	}

	scan := func(base string) ([]string, error) {
		var paths []string
		pvd := provider.NewDefaultDepProvider()
		rf := pvd.GetResourceFactory()
		err := fs.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if path == base {
				return nil
			}
			if info.IsDir() {
				// If a sub-directory contains an existing kustomization file add the
				// directory as a resource and do not decend into it.
				for _, kfilename := range konfig.RecognizedKustomizationFileNames() {
					if kpath := filepath.Join(path, kfilename); fs.Exists(kpath) && !fs.IsDir(kpath) {
						paths = append(paths, path)
						return filepath.SkipDir
					}
				}
				return nil
			}

			extension := filepath.Ext(path)
			if extension != ".yaml" && extension != ".yml" {
				return nil
			}

			fContents, err := fs.ReadFile(path)
			if err != nil {
				return err
			}

			if _, err := rf.SliceFromBytes(fContents); err != nil {
				return fmt.Errorf("failed to decode Kubernetes YAML from %s: %w", path, err)
			}
			paths = append(paths, path)
			return nil
		})
		return paths, err
	}

	abs, err := filepath.Abs(dirPath)
	if err != nil {
		return err
	}

	files, err := scan(abs)
	if err != nil {
		return err
	}

	kfile := filepath.Join(dirPath, konfig.DefaultKustomizationFileName())
	f, err := fs.Create(kfile)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}

	kus := kustypes.Kustomization{
		TypeMeta: kustypes.TypeMeta{
			APIVersion: kustypes.KustomizationVersion,
			Kind:       kustypes.KustomizationKind,
		},
	}

	var resources []string
	for _, file := range files {
		resources = append(resources, strings.Replace(file, abs, ".", 1))
	}

	kus.Resources = resources
	kd, err := yaml.Marshal(kus)
	if err != nil {
		return err
	}

	return os.WriteFile(kfile, kd, os.ModePerm)
}

func adaptSelector(selector *kustomize.Selector) (output *kustypes.Selector) {
	if selector != nil {
		output = &kustypes.Selector{}
		output.Gvk.Group = selector.Group
		output.Gvk.Kind = selector.Kind
		output.Gvk.Version = selector.Version
		output.Name = selector.Name
		output.Namespace = selector.Namespace
		output.LabelSelector = selector.LabelSelector
		output.AnnotationSelector = selector.AnnotationSelector
	}
	return
}

// TODO: remove mutex when kustomize fixes the concurrent map read/write panic
var kustomizeBuildMutex sync.Mutex

// secureBuildKustomization wraps krusty.MakeKustomizer with the following settings:
//  - specify resource sorting options
//  - secure on-disk FS denying operations outside root
//  - load files from outside the kustomization dir path
//    (but not outside root)
//  - disable plugins except for the builtin ones
func secureBuildKustomization(root, dirPath string, allowRemoteBases bool, doLegacyResourceSort bool) (_ resmap.ResMap, err error) {
	var fs filesys.FileSystem

	// Create secure FS for root with or without remote base support
	if allowRemoteBases {
		fs, err = securefs.MakeFsOnDiskSecureBuild(root)
		if err != nil {
			return nil, err
		}
	} else {
		fs, err = securefs.MakeFsOnDiskSecure(root)
		if err != nil {
			return nil, err
		}
	}

	// Temporary workaround for concurrent map read and map write bug
	// https://github.com/kubernetes-sigs/kustomize/issues/3659
	kustomizeBuildMutex.Lock()
	defer kustomizeBuildMutex.Unlock()

	// Kustomize tends to panic in unpredicted ways due to (accidental)
	// invalid object data; recover when this happens to ensure continuity of
	// operations
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered from kustomize build panic: %v", r)
		}
	}()

	buildOptions := &krusty.Options{
		DoLegacyResourceSort: doLegacyResourceSort,
		LoadRestrictions:     kustypes.LoadRestrictionsNone,
		PluginConfig:         kustypes.DisabledPluginConfig(),
	}

	k := krusty.MakeKustomizer(buildOptions)
	return k.Run(fs, dirPath)
}
