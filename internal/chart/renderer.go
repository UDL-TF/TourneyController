package chart

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/releaseutil"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// Renderer materializes Helm manifests and applies them via the dynamic client.
type Renderer struct {
	chart     *chart.Chart
	baseVals  chartutil.Values
	namespace string
	dynamic   dynamic.Interface
	mapper    meta.ResettableRESTMapper
}

// NewRenderer loads the chart, initializes Kubernetes helpers, and prepares for reconciliation.
func NewRenderer(restCfg *rest.Config, chartPath, valuesFile, namespace string) (*Renderer, error) {
	ch, err := loadChart(chartPath)
	if err != nil {
		return nil, err
	}

	var base chartutil.Values
	if valuesFile != "" {
		if _, err := os.Stat(valuesFile); err == nil {
			base, err = chartutil.ReadValuesFile(valuesFile)
			if err != nil {
				return nil, fmt.Errorf("read values file %s: %w", valuesFile, err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat values file %s: %w", valuesFile, err)
		}
	}
	if base == nil {
		base = chartutil.Values{}
	}

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	disco, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	return &Renderer{
		chart:     ch,
		baseVals:  base,
		namespace: namespace,
		dynamic:   dyn,
		mapper:    mapper,
	}, nil
}

func loadChart(chartPath string) (*chart.Chart, error) {
	if strings.HasPrefix(chartPath, "oci://") {
		return loadChartFromOCI(chartPath)
	}

	ch, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("load chart %s: %w", chartPath, err)
	}
	return ch, nil
}

func loadChartFromOCI(ref string) (*chart.Chart, error) {
	client, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create registry client: %w", err)
	}

	result, err := client.Pull(ref)
	if err != nil {
		return nil, fmt.Errorf("pull chart %s: %w", ref, err)
	}
	if result.Chart == nil || len(result.Chart.Data) == 0 {
		return nil, fmt.Errorf("pulled chart %s contains no data", ref)
	}

	reader := bytes.NewReader(result.Chart.Data)
	ch, err := loader.LoadArchive(reader)
	if err != nil {
		return nil, fmt.Errorf("load oci chart %s: %w", ref, err)
	}
	return ch, nil
}

// Apply renders the chart with overrides and upserts every resource.
func (r *Renderer) Apply(ctx context.Context, releaseName string, overrides chartutil.Values) error {
	objects, err := r.renderObjects(releaseName, overrides)
	if err != nil {
		return err
	}

	for _, obj := range objects {
		if err := r.applyObject(ctx, obj.DeepCopy()); err != nil {
			return err
		}
	}
	return nil
}

// Delete renders the chart and removes each resource.
func (r *Renderer) Delete(ctx context.Context, releaseName string, overrides chartutil.Values) error {
	objects, err := r.renderObjects(releaseName, overrides)
	if err != nil {
		return err
	}

	for i := len(objects) - 1; i >= 0; i-- {
		if err := r.deleteObject(ctx, objects[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *Renderer) renderObjects(releaseName string, overrides chartutil.Values) ([]*unstructured.Unstructured, error) {
	values := r.mergeValues(overrides)

	releaseOpts := chartutil.ReleaseOptions{
		Name:      releaseName,
		Namespace: r.namespace,
		Revision:  1,
		IsInstall: true,
		IsUpgrade: true,
	}

	renderVals, err := chartutil.ToRenderValues(r.chart, values, releaseOpts, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("build render values: %w", err)
	}

	eng := engine.Engine{}
	manifests, err := eng.Render(r.chart, renderVals)
	if err != nil {
		return nil, fmt.Errorf("render helm chart: %w", err)
	}

	var objects []*unstructured.Unstructured
	for name, manifest := range manifests {
		if strings.HasSuffix(name, "NOTES.txt") {
			continue
		}
		split := releaseutil.SplitManifests(manifest)
		for _, fragment := range split {
			dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(fragment), 4096)
			for {
				raw := map[string]interface{}{}
				if err := dec.Decode(&raw); err != nil {
					if err == io.EOF {
						break
					}
					return nil, fmt.Errorf("decode manifest %s: %w", name, err)
				}
				if len(raw) == 0 {
					continue
				}
				obj := &unstructured.Unstructured{Object: raw}
				if obj.GetKind() == "" {
					continue
				}
				if obj.GetKind() == "List" {
					items, _, _ := unstructured.NestedSlice(obj.Object, "items")
					for _, item := range items {
						if typed, ok := item.(map[string]interface{}); ok {
							objects = append(objects, &unstructured.Unstructured{Object: typed})
						}
					}
					continue
				}
				objects = append(objects, obj)
			}
		}
	}

	return objects, nil
}

func (r *Renderer) applyObject(ctx context.Context, obj *unstructured.Unstructured) error {
	mapping, err := r.restMapping(obj.GroupVersionKind())
	if err != nil {
		return err
	}

	resource, err := r.resourceInterface(mapping, obj)
	if err != nil {
		return err
	}

	existing, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			_, createErr := resource.Create(ctx, obj, metav1.CreateOptions{})
			return createErr
		}
		return err
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = resource.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func (r *Renderer) deleteObject(ctx context.Context, obj *unstructured.Unstructured) error {
	mapping, err := r.restMapping(obj.GroupVersionKind())
	if err != nil {
		return err
	}

	resource, err := r.resourceInterface(mapping, obj)
	if err != nil {
		return err
	}

	if err := resource.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *Renderer) resourceInterface(mapping *meta.RESTMapping, obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		namespace := obj.GetNamespace()
		if namespace == "" {
			namespace = r.namespace
			obj.SetNamespace(namespace)
		}
		return r.dynamic.Resource(mapping.Resource).Namespace(namespace), nil
	}
	return r.dynamic.Resource(mapping.Resource), nil
}

func (r *Renderer) restMapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mapping, err := r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		r.mapper.Reset()
		mapping, err = r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("resolve REST mapping for %s: %w", gvk.String(), err)
		}
	}
	return mapping, nil
}

func (r *Renderer) mergeValues(overrides chartutil.Values) chartutil.Values {
	merged := cloneValues(r.chart.Values)
	merged = chartutil.CoalesceTables(cloneValues(r.baseVals), merged)
	if overrides != nil {
		merged = chartutil.CoalesceTables(cloneValues(overrides), merged)
	}
	return merged
}

func cloneValues(vals chartutil.Values) chartutil.Values {
	if vals == nil {
		return chartutil.Values{}
	}
	return chartutil.Values(deepCopyMap(map[string]interface{}(vals)))
}

func deepCopyMap(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = deepCopyValue(v)
	}
	return dst
}

func deepCopySlice(src []interface{}) []interface{} {
	dst := make([]interface{}, len(src))
	for i, v := range src {
		dst[i] = deepCopyValue(v)
	}
	return dst
}

func deepCopyValue(val interface{}) interface{} {
	switch typed := val.(type) {
	case map[string]interface{}:
		return deepCopyMap(typed)
	case []interface{}:
		return deepCopySlice(typed)
	default:
		return typed
	}
}
