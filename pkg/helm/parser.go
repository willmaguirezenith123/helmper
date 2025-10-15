package helm

import (
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/ChristofferNissen/helmper/pkg/image"
	"github.com/ChristofferNissen/helmper/pkg/util/ternary"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
)

// traverse helm chart values to determine if condition is met
func ConditionMet(condition string, values map[string]any) bool {
	pos := values
	enabled := false
	for _, e := range strings.Split(condition, ".") {
		switch v := pos[e].(type) {
		case string:
			enabled = v == "true"
		case bool:
			enabled = v
		case map[string](any):
			pos = v
		case interface{}:
			pos = pos[e].(map[string]any)
		}
	}
	return enabled
}

// traverse helm chart values data structure
func findImageReferencesAcc(data map[string]any, values map[string]any, useCustomValues bool, acc string) map[*image.Image][]string {
	res := make(map[*image.Image][]string)

	i := to.Ptr(image.Image{})
	for k, v := range data {
		switch v := v.(type) {

		// yaml key-value pair value type
		case bool:
			switch k {
			case "useDigest":
				i.UseDigest = v
			}
		case string:
			found := true

			switch k {
			case "registry":
				switch useCustomValues {
				case true:
					s, ok := values[k].(string)
					if ok {
						i.Registry = s
					} else {
						i.Registry = v
					}
				case false:
					i.Registry = v
				}
			case "repository":
				switch useCustomValues {
				case true:
					s, ok := values[k].(string)
					if ok {
						i.Repository = s
					} else {
						i.Repository = v
					}
				case false:
					i.Repository = v
				}
			case "image":
				switch useCustomValues {
				case true:
					s, ok := values[k].(string)
					if ok {
						i.Repository = s
					} else {
						i.Repository = v
					}
				case false:
					i.Repository = v
				}
			case "tag":
				switch useCustomValues {
				case true:
					s, ok := values[k].(string)
					if ok {
						i.Tag = s
					} else {
						i.Tag = v
					}
				case false:
					i.Tag = v
				}
			case "digest":
				switch useCustomValues {
				case true:
					s, ok := values[k].(string)
					if ok {
						i.Digest = s
					} else {
						i.Digest = v
					}
				case false:
					i.Digest = v
				}
			case "sha":
				switch useCustomValues {
				case true:
					s, ok := values[k].(string)
					if ok {
						i.Digest = s
					} else {
						i.Digest = v
					}
				case false:
					i.Digest = v
				}
			default:
				found = false
			}

			if found {
				res[i] = append(res[i], fmt.Sprintf("%s.%s", acc, k))
			}

		// nested yaml object
		case map[string]any:
			// same path in yaml

			// Only parsed enabled sections
			enabled := true
			for k1, v1 := range v {
				if k1 == "enabled" {
					switch value := v1.(type) {
					case string:
						enabled = value == "true"
					case bool:
						enabled = ConditionMet(k1, values[k].(map[string]any))
					}
				}
			}

			// if enabled, parse nested section
			if enabled {
				path := ternary.Ternary(acc == "", k, fmt.Sprintf("%s.%s", acc, k))
				nestedRes := findImageReferencesAcc(v, values[k].(map[string]any), useCustomValues, path)
				for k, v := range nestedRes {
					res[k] = v
				}
			}
		}
	}

	return res
}

func findImageReferences(data map[string]any, values map[string]any, useCustomValues bool) map[*image.Image][]string {
	return findImageReferencesAcc(data, values, useCustomValues, "")
}

// traverse helm chart values data structure
func replaceImageReferences(data map[string]any, reg string, prefixSource bool) {

	// For images we do not use the prefix and suffix of the registry
	reg, _ = strings.CutPrefix(reg, "oci://")

	convert := func(val string) string {
		ref, err := reference.ParseAnyReference(val)
		if err != nil {
			return ""
		}
		r := ref.(reference.Named)
		dom := reference.Domain(r)

		source := strings.Split(dom, ":")[0]
		source = strings.Split(source, ".")[0]
		source = "/" + source
		if prefixSource {
			reg = reg + source
		}

		if strings.Contains(val, dom) {
			return strings.Replace(ref.String(), dom, reg, 1)
		} else {
			if strings.HasPrefix(ref.String(), "docker.io/library/") {
				return reg + "/library/" + val
			}
			return reg + "/" + val
		}
	}

	old, ok := data["registry"].(string)
	if ok {
		data["registry"] = reg
		if prefixSource {
			repository, ok := data["repository"].(string)
			if ok {
				source := strings.Split(old, ":")[0]
				source = strings.Split(source, ".")[0]
				old = source + "/" + repository

				data["repository"] = old
			}
		}
		return
	}

	image, ok := data["image"].(string)
	if ok {
		data["image"] = convert(image)
		return
	}

	repository, ok := data["repository"].(string)
	if ok {
		data["repository"] = convert(repository)
		return
	}

	for k, v := range data {
		switch v.(type) {
		// nested yaml object
		case map[string]any:
			replaceImageReferences(data[k].(map[string]any), reg, prefixSource)
		}
	}
}

// renderHelmTemplate renders a helm chart using helm template action and returns the manifests
func renderHelmTemplate(chartRef *chart.Chart, values map[string]any, settings *cli.EnvSettings, releaseName string, namespace string, kubeVersion string) (string, error) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, "memory", log.Printf); err != nil {
		return "", fmt.Errorf("failed to initialize action configuration: %w", err)
	}

	install := action.NewInstall(actionConfig)
	install.DryRun = true
	install.ReleaseName = releaseName
	install.Namespace = namespace
	install.Replace = true
	install.ClientOnly = true
	install.KubeVersion = &chartutil.KubeVersion{Version: kubeVersion}

	// Render the chart with the provided values
	release, err := install.Run(chartRef, values)
	if err != nil {
		return "", fmt.Errorf("failed to render helm template: %w", err)
	}

	return release.Manifest, nil
}

// findImageReferencesFromManifest extracts image references from rendered Kubernetes manifests
func findImageReferencesFromManifest(manifest string) (map[*image.Image][]string, error) {
	result := make(map[*image.Image][]string)

	// Split manifest into individual documents
	documents := strings.Split(manifest, "---")

	for _, doc := range documents {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		// Parse the YAML document
		var k8sResource map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &k8sResource); err != nil {
			continue // Skip invalid YAML
		}

		// Extract images from this document
		images := extractImagesFromResource(k8sResource)
		for _, imgStr := range images {
			img, err := image.RefToImage(imgStr)
			if err != nil {
				continue // Skip invalid image references
			}

			// Generate a path based on the resource type and name
			path := generateResourcePath(k8sResource, imgStr)
			result[&img] = append(result[&img], path)
		}
	}

	return result, nil
}

// extractImagesFromResource recursively extracts image references from a Kubernetes resource
func extractImagesFromResource(resource interface{}) []string {
	var images []string

	switch v := resource.(type) {
	case map[string]interface{}:
		for key, value := range v {
			if key == "image" {
				if imgStr, ok := value.(string); ok && imgStr != "" {
					images = append(images, imgStr)
				}
			} else {
				images = append(images, extractImagesFromResource(value)...)
			}
		}
	case []interface{}:
		for _, item := range v {
			images = append(images, extractImagesFromResource(item)...)
		}
	}

	return images
}

// generateResourcePath creates a descriptive path for the image reference
func generateResourcePath(resource map[string]interface{}, imageRef string) string {
	kind := "unknown"
	name := "unknown"

	if k, ok := resource["kind"].(string); ok {
		kind = k
	}

	if metadata, ok := resource["metadata"].(map[string]interface{}); ok {
		if n, ok := metadata["name"].(string); ok {
			name = n
		}
	}

	return fmt.Sprintf("%s/%s/image=%s", kind, name, imageRef)
}
