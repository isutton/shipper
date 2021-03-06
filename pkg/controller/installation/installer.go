package installation

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	shipperv1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	shipperchart "github.com/bookingcom/shipper/pkg/chart"
	"github.com/bookingcom/shipper/pkg/controller/janitor"
)

type DynamicClientBuilderFunc func(gvk *schema.GroupVersionKind, restConfig *rest.Config, cluster *shipperv1.Cluster) dynamic.Interface

// Installer is an object that knows how to install Helm charts directly into
// Kubernetes clusters.
type Installer struct {
	fetchChart shipperchart.FetchFunc

	Release            *shipperv1.Release
	InstallationTarget *shipperv1.InstallationTarget
	Scheme             *runtime.Scheme
}

// NewInstaller returns a new Installer.
func NewInstaller(chartFetchFunc shipperchart.FetchFunc,
	release *shipperv1.Release,
	it *shipperv1.InstallationTarget,
) *Installer {
	return &Installer{
		fetchChart:         chartFetchFunc,
		Release:            release,
		InstallationTarget: it,
		Scheme:             kubescheme.Scheme,
	}
}

// renderManifests returns a list of rendered manifests for the given release and
// cluster, or an error.
func (i *Installer) renderManifests(_ *shipperv1.Cluster) ([]string, error) {
	rel := i.Release
	chart, err := i.fetchChart(rel.Environment.Chart)
	if err != nil {
		return nil, RenderManifestError(err)
	}

	rendered, err := shipperchart.Render(
		chart,
		rel.GetName(),
		rel.GetNamespace(),
		rel.Environment.Values,
	)

	if err != nil {
		err = RenderManifestError(err)
	}

	for _, v := range rendered {
		glog.V(10).Infof("Rendered object:\n%s", v)
	}

	return rendered, err
}

// buildResourceClient returns a ResourceClient suitable to manipulate the kind
// of resource represented by the given GroupVersionKind at the given Cluster.
func (i *Installer) buildResourceClient(
	cluster *shipperv1.Cluster,
	client kubernetes.Interface,
	restConfig *rest.Config,
	dynamicClientBuilder DynamicClientBuilderFunc,
	gvk *schema.GroupVersionKind,
) (dynamic.ResourceInterface, error) {
	dynamicClient := dynamicClientBuilder(gvk, restConfig, cluster)

	// From the list of resources the target cluster knows about, find the resource for the
	// kind of object we have at hand.
	var resource *metav1.APIResource
	gv := gvk.GroupVersion().String()
	if resources, err := client.Discovery().ServerResourcesForGroupVersion(gv); err != nil {
		return nil, err
	} else {
		for _, e := range resources.APIResources {
			if e.Kind == gvk.Kind {
				resource = &e
				break
			}
		}
		if resource == nil {
			return nil, fmt.Errorf("resource %s not found", gvk.Kind)
		}
	}

	// If it gets to this point, it means we have a resource, so we can create a
	// client for it scoping to the application's namespace. The namespace can be
	// ignored if creating, for example, objects that aren't bound to a namespace.
	resourceClient := dynamicClient.Resource(resource, i.Release.Namespace)
	return resourceClient, nil
}

func (i *Installer) patchDeployment(
	d *appsv1.Deployment,
	labelsToInject map[string]string,
	ownerReference *metav1.OwnerReference,
) runtime.Object {

	d.OwnerReferences = []metav1.OwnerReference{*ownerReference}

	replicas := int32(0)
	d.Spec.Replicas = &replicas

	newLabels := d.Labels
	if newLabels == nil {
		newLabels = map[string]string{}
	}

	for k, v := range labelsToInject {
		newLabels[k] = v
	}
	d.SetLabels(newLabels)

	// Patch .spec.selector
	var newSelector *metav1.LabelSelector
	if d.Spec.Selector != nil {
		newSelector = d.Spec.Selector.DeepCopy()
	} else {
		newSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{},
		}
	}

	for k, v := range labelsToInject {
		newSelector.MatchLabels[k] = v
	}
	d.Spec.Selector = newSelector

	podTemplateLabels := d.Spec.Template.Labels
	for k, v := range labelsToInject {
		podTemplateLabels[k] = v
	}
	d.Spec.Template.SetLabels(podTemplateLabels)

	return d
}

func (i *Installer) patchService(
	s *corev1.Service,
	labelsToInject map[string]string,
	ownerReference *metav1.OwnerReference,
) runtime.Object {
	s.SetOwnerReferences([]metav1.OwnerReference{*ownerReference})
	s.SetLabels(mergeLabels(s.GetLabels(), labelsToInject))
	return s
}

func (i *Installer) patchUnstructured(
	o *unstructured.Unstructured,
	labelsToInject map[string]string,
	ownerReference *metav1.OwnerReference,
) runtime.Object {

	o.SetOwnerReferences([]metav1.OwnerReference{*ownerReference})

	labels := o.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	for k, v := range labelsToInject {
		labels[k] = v
	}
	o.SetLabels(labels)
	return o
}

func (i *Installer) patchObject(
	object runtime.Object,
	labelsToInject map[string]string,
	ownerReference *metav1.OwnerReference,
) runtime.Object {
	switch o := object.(type) {
	case *appsv1.Deployment:
		return i.patchDeployment(o, labelsToInject, ownerReference)
	case *corev1.Service:
		return i.patchService(o, labelsToInject, ownerReference)
	default:
		unstructuredObj := &unstructured.Unstructured{}
		err := i.Scheme.Convert(object, unstructuredObj, nil)
		if err != nil {
			panic(err)
		}
		return i.patchUnstructured(unstructuredObj, labelsToInject, ownerReference)
	}
}

// installManifests attempts to install the manifests on the specified cluster.
func (i *Installer) installManifests(
	cluster *shipperv1.Cluster,
	client kubernetes.Interface,
	restConfig *rest.Config,
	dynamicClientBuilderFunc DynamicClientBuilderFunc,
	manifests []string,
) error {

	var configMap *corev1.ConfigMap
	var createdConfigMap *corev1.ConfigMap
	var existingConfigMap *corev1.ConfigMap
	var err error

	if configMap, err = janitor.CreateConfigMapAnchor(i.InstallationTarget); err != nil {
		return NewCreateResourceError("error creating anchor config map: %s ", err)
	} else if existingConfigMap, err = client.CoreV1().ConfigMaps(i.Release.Namespace).Get(configMap.Name, metav1.GetOptions{}); err != nil && !errors.IsNotFound(err) {
		return NewGetResourceError(`error getting anchor %q: %s`, configMap.Name, err)
	} else if err != nil { // errors.IsNotFound(err) == true
		if createdConfigMap, err = client.CoreV1().ConfigMaps(configMap.Namespace).Create(configMap); err != nil {
			return NewCreateResourceError(
				`error creating anchor resource %s "%s/%s": %s`,
				configMap.Kind, configMap.Namespace, configMap.Name, err)
		}
	} else {
		createdConfigMap = existingConfigMap
	}

	// Create the OwnerReference for the manifest objects.
	ownerReference := janitor.ConfigMapAnchorToOwnerReference(createdConfigMap)

	// Try to install all the rendered objects in the target cluster. We should
	// fail in the first error to report that this cluster has an issue. Since the
	// InstallationTarget.Status represent a per cluster status with a scalar
	// value, we don't try to install other objects for now.
	for _, manifest := range manifests {
		decodedObj, gvk, err :=
			kubescheme.Codecs.
				UniversalDeserializer().
				Decode([]byte(manifest), nil, nil)

		if err != nil {
			return NewDecodeManifestError("error decoding manifest: %s", err)
		}

		// We label final objects with Release labels so that we can find/filter them
		// later in Capacity and Installation controllers.
		// This may overwrite some of the pre-existing labels. It's not ideal but with
		// current implementation we require that shipperv1.ReleaseLabel is propagated
		// correctly. This may be subject to change.
		labelsToInject := i.Release.Labels
		decodedObj = i.patchObject(decodedObj, labelsToInject, &ownerReference)

		// ResourceClient.Create() requires an Unstructured object to work with, so we
		// need to convert.
		obj := &unstructured.Unstructured{}
		err = i.Scheme.Convert(decodedObj, obj, nil)
		if err != nil {
			return NewConvertUnstructuredError("error converting object to unstructured: %s", err)
		}

		// Once we've gathered enough information about the document we want to
		// install, we're able to build a resource client to interact with the target
		// cluster.
		resourceClient, err := i.buildResourceClient(cluster, client, restConfig, dynamicClientBuilderFunc, gvk)
		if err != nil {
			return NewResourceClientError("error building resource client: %s", err)
		}

		// "fetch-and-create-or-update" strategy in here; this is required to
		// overcome an issue in Kubernetes where a "create-or-update" strategy
		// leads to exceeding quotas when those are enabled very quickly,
		// since Kubernetes machinery first increase quota usage and then
		// attempts to create the resource, taking some time to re-sync
		// the quota information when objects can't be created since they
		// already exist.
		existingObj, err := resourceClient.Get(obj.GetName(), metav1.GetOptions{})

		// Any error other than NotFound is not recoverable from this point on.
		if err != nil && !errors.IsNotFound(err) {
			return NewGetResourceError(`error getting resource %s '%s/%s': %s`, obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}

		// If have an error here, it means it is NotFound, so proceed to
		// create the object on the application cluster.
		if err != nil {
			_, err = resourceClient.Create(obj)
			if err != nil {
				return NewCreateResourceError(`error creating resource %s "%s/%s": %s`, obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
			continue
		}

		// We inject a Namespace object in the objects to be installed for a
		// particular Release; we don't want to continue if the Namespace already
		// exists.
		if gvk := existingObj.GroupVersionKind(); gvk.Kind == "Namespace" {
			continue
		}

		// If the existing object was stamped with the driving release,
		// continue to the next manifest.
		if releaseLabelValue, ok := existingObj.GetLabels()[shipperv1.ReleaseLabel]; ok && releaseLabelValue == i.Release.Name {
			continue
		} else if !ok {
			return NewIncompleteReleaseError(`Release "%s/%s" misses the required label %q`, existingObj.GetNamespace(), existingObj.GetName(), shipperv1.ReleaseLabel)
		}

		ownerReferenceFound := false
		for _, o := range existingObj.GetOwnerReferences() {
			if reflect.DeepEqual(o, ownerReference) {
				ownerReferenceFound = true
			}
		}
		if !ownerReferenceFound {
			ownerReferences := append(existingObj.GetOwnerReferences(), ownerReference)
			sort.Slice(ownerReferences, func(i, j int) bool {
				return ownerReferences[i].Name < ownerReferences[j].Name
			})
			existingObj.SetOwnerReferences(ownerReferences)
		}
		existingObj.SetLabels(obj.GetLabels())
		existingObj.SetAnnotations(obj.GetAnnotations())
		existingUnstructuredObj := existingObj.UnstructuredContent()
		newUnstructuredObj := obj.UnstructuredContent()
		switch decodedObj.(type) {
		case *corev1.Service:
			// Copy over clusterIP from existing object's .spec to the
			// rendered one.
			if clusterIP, ok := unstructured.NestedString(existingUnstructuredObj, "spec", "clusterIP"); ok {
				unstructured.SetNestedField(newUnstructuredObj, clusterIP, "spec", "clusterIP")
			}
		}
		unstructured.SetNestedField(existingUnstructuredObj, newUnstructuredObj["spec"], "spec")
		existingObj.SetUnstructuredContent(existingUnstructuredObj)
		if _, clientErr := resourceClient.Update(existingObj); clientErr != nil {
			return NewUpdateResourceError(`error updating resource %s "%s/%s": %s`,
				existingObj.GetKind(), obj.GetNamespace(), obj.GetName(), clientErr)
		}
	}

	return nil
}

// installRelease attempts to install the given release on the given cluster.
func (i *Installer) installRelease(
	cluster *shipperv1.Cluster,
	client kubernetes.Interface,
	restConfig *rest.Config,
	dynamicClientBuilder DynamicClientBuilderFunc,
) error {

	renderedManifests, err := i.renderManifests(cluster)
	if err != nil {
		return err
	}

	return i.installManifests(cluster, client, restConfig, dynamicClientBuilder, renderedManifests)
}

// mergeLabels takes to sets of labels and merge them into another set.
//
// Values of the second set overwrite values from the first one.
func mergeLabels(a map[string]string, b map[string]string) map[string]string {

	labels := make(map[string]string)

	for k, v := range a {
		labels[k] = v
	}

	for k, v := range b {
		labels[k] = v
	}

	return labels
}
