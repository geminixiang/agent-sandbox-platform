package kubernetes

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

var (
	claimResource    = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
	warmPoolResource = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}
	sandboxResource  = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
)

type resourceClient interface {
	Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, metav1.CreateOptions) (*unstructured.Unstructured, error)
	Get(context.Context, schema.GroupVersionResource, string, string, metav1.GetOptions) (*unstructured.Unstructured, error)
	List(context.Context, schema.GroupVersionResource, string, metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Delete(context.Context, schema.GroupVersionResource, string, string, metav1.DeleteOptions) error
}

type podClient interface {
	Get(context.Context, string, string, metav1.GetOptions) (*corev1.Pod, error)
}

type dynamicResources struct{ client dynamic.Interface }

func (d dynamicResources) Create(ctx context.Context, resource schema.GroupVersionResource, namespace string, value *unstructured.Unstructured, options metav1.CreateOptions) (*unstructured.Unstructured, error) {
	return d.client.Resource(resource).Namespace(namespace).Create(ctx, value, options)
}
func (d dynamicResources) Get(ctx context.Context, resource schema.GroupVersionResource, namespace, name string, options metav1.GetOptions) (*unstructured.Unstructured, error) {
	return d.client.Resource(resource).Namespace(namespace).Get(ctx, name, options)
}
func (d dynamicResources) List(ctx context.Context, resource schema.GroupVersionResource, namespace string, options metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return d.client.Resource(resource).Namespace(namespace).List(ctx, options)
}
func (d dynamicResources) Delete(ctx context.Context, resource schema.GroupVersionResource, namespace, name string, options metav1.DeleteOptions) error {
	return d.client.Resource(resource).Namespace(namespace).Delete(ctx, name, options)
}

type corePods struct{ client coreclient.CoreV1Interface }

func (c corePods) Get(ctx context.Context, namespace, name string, options metav1.GetOptions) (*corev1.Pod, error) {
	return c.client.Pods(namespace).Get(ctx, name, options)
}
