# Fix: Incorrect Image Registry Extraction from Helm Charts

## Issue Description

Helmper incorrectly extracts container image references from Helm charts when the chart uses template logic to modify registry URLs during rendering. This results in images being extracted with the wrong registry prefix, causing image availability checks to fail and incorrect import operations.

## Problem Details

### Root Cause
Helmper currently parses the static `values.yaml` file from Helm charts using the `findImageReferences()` function, which only examines the raw chart values. However, many Helm charts (like Kyverno) use template logic that dynamically constructs image registry URLs during the `helm template` rendering process.

**Specific Kyverno Issue Analysis:**

In the Kyverno Helm chart, images are constructed using a sophisticated template helper function in `/templates/_helpers/_image.tpl`:

```go
{{- define "kyverno.image" -}}
{{- $tag := default .defaultTag .image.tag -}}
{{- $imageRegistry := default (default .image.defaultRegistry .globalRegistry) .image.registry -}}
{{- if $imageRegistry -}}
  {{- print $imageRegistry "/" (required "An image repository is required" .image.repository) ":" $tag -}}
{{- else -}}
  {{- print (required "An image repository is required" .image.repository) ":" $tag -}}
{{- end -}}
{{- end -}}
```

The issue occurs because:

1. **Static values.yaml shows**: `repository: kyverno/kyverno` with `defaultRegistry: reg.kyverno.io`
2. **Helmper's parser only sees**: The `repository` field as `kyverno/kyverno`
3. **Template logic applies**: `$imageRegistry` gets set to `reg.kyverno.io` from `defaultRegistry`
4. **Final rendered image**: `reg.kyverno.io/kyverno/kyverno:v1.15.2`

However, Helmper's `findImageReferences()` function only looks for static fields like `registry`, `repository`, and `tag` in the values structure, completely missing the `defaultRegistry` field and the template logic that combines them.

### Observed Behavior
When processing the Kyverno chart v3.5.2:
- **Helmper extracts**: `docker.io/kyverno/kyverno:v1.15.2` (incorrectly parsed from `repository: kyverno/kyverno`)
- **Actual deployed image**: `reg.kyverno.io/kyverno/kyverno:v1.15.2` (correctly rendered by template)

This discrepancy causes:
1. Image availability checks to fail (looking for images in the wrong registry)
2. Incorrect image references in import operations
3. False negatives when the tool reports "Image not available"

### Technical Details

The Kyverno chart defines images like this in `values.yaml`:
```yaml
admissionController:
  container:
    image:
      registry: ~                          # Can override defaultRegistry
      defaultRegistry: reg.kyverno.io      # Default registry when registry is not set
      repository: kyverno/kyverno          # Repository path
      tag: ~                               # Defaults to appVersion
```

The template then uses the helper function to construct the full image reference, applying precedence:
1. `.globalRegistry` (global override)
2. `.image.registry` (component-specific override) 
3. `.image.defaultRegistry` (component default)

This pattern is common in modern Helm charts for flexibility, but Helmper's static parsing misses this dynamic construction.

## Reproduction Steps

1. Create a test configuration file:
```yaml
# test.config
charts:
    - name: kyverno
      repo:
        name: kyverno
        url: https://kyverno.github.io/kyverno
      version: 3.5.2
parser:
    useCustomValues: true
registries:
    - insecure: true
      name: localhost
      plainHTTP: true
      sourcePrefix: true
      url: localhost:5000
```

2. Run helmper:
```bash
helmper --f test.config
```

3. Compare with actual helm template output:
```bash
helm template kyverno kyverno/kyverno -n kyverno | yq '..|.image? | select(.)' | sort -u
```

**Expected**: Images should match between helmper and `helm template`  
**Actual**: Helmper shows `docker.io/kyverno/*` while `helm template` shows `reg.kyverno.io/kyverno/*`

## Root Cause Analysis

The issue stems from Kyverno's template architecture:

1. **Values Definition**: In `values.yaml`, images are defined with `defaultRegistry` fields:
   ```yaml
   admissionController:
     container:
       image:
         defaultRegistry: reg.kyverno.io
         repository: kyverno/kyverno
   ```

2. **Template Logic**: The `kyverno.image` helper in `_helpers/_image.tpl` constructs the full image reference:
   ```go
   {{- $imageRegistry := default (default .image.defaultRegistry .globalRegistry) .image.registry -}}
   {{- if $imageRegistry -}}
     {{- print $imageRegistry "/" .image.repository ":" $tag -}}
   ```

3. **Helmper's Limitation**: The `findImageReferences()` function only looks for static fields (`registry`, `repository`, `tag`) and misses:
   - The `defaultRegistry` field
   - Template logic that combines registry + repository
   - Dynamic image construction during rendering

This pattern affects all Kyverno components (admission-controller, background-controller, cleanup-controller, reports-controller, and kyvernopre).

## Proposed Solution

Replace the static values parsing with actual Helm template rendering to extract images from the manifests that would be deployed.

### Implementation Details

#### 1. Add Template Rendering Function
Add a new function in `pkg/helm/parser.go` to render Helm templates:

```go
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

    release, err := install.Run(chartRef, values)
    if err != nil {
        return "", fmt.Errorf("failed to render helm template: %w", err)
    }

    return release.Manifest, nil
}
```

#### 2. Add Manifest Image Parser
Add a function to extract images from rendered Kubernetes manifests:

```go
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
```

#### 3. Update Chart Processing Logic
In `pkg/helm/chartOption.go`, replace the existing image extraction logic around line 366:

```go
// Before (current code):
imageMap := findImageReferences(chart.Values, values, co.UseCustomValues)

// After (proposed fix):
var imageMap map[*image.Image][]string
releaseName := fmt.Sprintf("helmper-%s", c.Name)
namespace := "default"
manifest, err := renderHelmTemplate(chart, values, co.Settings, releaseName, namespace, args.K8SVersion)
if err != nil {
    slog.Info("failed to render helm template, falling back to values parsing", slog.String("chart", c.Name), slog.String("error", err.Error()))
    // Fallback to original method if template rendering fails
    imageMap = findImageReferences(chart.Values, values, co.UseCustomValues)
} else {
    // Use template-based image extraction
    imageMap, err = findImageReferencesFromManifest(manifest)
    if err != nil {
        slog.Info("failed to extract images from manifest, falling back to values parsing", slog.String("chart", c.Name), slog.String("error", err.Error()))
        // Fallback to original method
        imageMap = findImageReferences(chart.Values, values, co.UseCustomValues)
    }
}
```

### Required Import Additions

Add these imports to `pkg/helm/parser.go`:
```go
import (
    "helm.sh/helm/v3/pkg/action"
    "helm.sh/helm/v3/pkg/chart"
    "helm.sh/helm/v3/pkg/chartutil"
    "helm.sh/helm/v3/pkg/cli"
    "gopkg.in/yaml.v3"
)
```

## Benefits

1. **Accuracy**: Images are extracted from actual rendered manifests, not static values
2. **Compatibility**: Works with any chart that uses template logic for image references
3. **Backward Compatibility**: Falls back to original method if template rendering fails
4. **Robustness**: Handles complex chart transformations and conditional logic

## Validation

After implementing the fix, the Kyverno chart correctly extracts:
- `reg.kyverno.io/kyverno/kyverno:v1.15.2`
- `reg.kyverno.io/kyverno/background-controller:v1.15.2`
- `reg.kyverno.io/kyverno/cleanup-controller:v1.15.2`
- `reg.kyverno.io/kyverno/reports-controller:v1.15.2`
- `reg.kyverno.io/kyverno/kyvernopre:v1.15.2`

This matches exactly with `helm template` output.

## Related Files

- `pkg/helm/parser.go` - Add new template rendering functions
- `pkg/helm/chartOption.go` - Update image extraction logic
- `pkg/helm/types.go` - No changes needed
- `pkg/image/` - No changes needed (existing image parsing works correctly)

## Testing

The fix has been tested with:
- **Kyverno chart v3.5.2** - Complex template logic with `defaultRegistry` pattern
- **Charts with static image references** - Backward compatibility verified  
- **Edge cases where template rendering fails** - Fallback behavior works correctly

### Before/After Comparison

**Before Fix (Helmper output)**:
```
{"level":"INFO","msg":"Image not available. will be excluded from import...","image":"docker.io/kyverno/kyverno:v1.15.2"}
{"level":"INFO","msg":"Image not available. will be excluded from import...","image":"docker.io/kyverno/background-controller:v1.15.2"}
```

**After Fix (Helmper output)**:
```
| reg.kyverno.io/kyverno/kyverno:v1.15.2               | Deployment/kyverno-admission-controller/image=reg.kyverno.io/kyverno/kyverno:v1.15.2 |
| reg.kyverno.io/kyverno/background-controller:v1.15.2 | Deployment/kyverno-background-controller/image=reg.kyverno.io/kyverno/background-controller:v1.15.2 |
```

**Helm Template Reference**:
```bash
$ helm template kyverno kyverno/kyverno -n kyverno | yq '..|.image? | select(.)' | sort -u
reg.kyverno.io/kyverno/background-controller:v1.15.2
reg.kyverno.io/kyverno/cleanup-controller:v1.15.2
reg.kyverno.io/kyverno/kyverno:v1.15.2
reg.kyverno.io/kyverno/kyvernopre:v1.15.2
reg.kyverno.io/kyverno/reports-controller:v1.15.2
```

✅ **Perfect Match**: After the fix, Helmper extracts exactly the same images as `helm template`.

This solution ensures that Helmper accurately reflects the actual container images that would be deployed by Helm, rather than just the static configuration values. It handles the sophisticated templating patterns used by modern Helm charts like Kyverno.