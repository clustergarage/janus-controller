// Code generated by client-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
	scheme "clustergarage.io/janus-controller/pkg/client/clientset/versioned/scheme"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
)

// JanusWatchersGetter has a method to return a JanusWatcherInterface.
// A group's client should implement this interface.
type JanusWatchersGetter interface {
	JanusWatchers(namespace string) JanusWatcherInterface
}

// JanusWatcherInterface has methods to work with JanusWatcher resources.
type JanusWatcherInterface interface {
	Create(*v1alpha1.JanusWatcher) (*v1alpha1.JanusWatcher, error)
	Update(*v1alpha1.JanusWatcher) (*v1alpha1.JanusWatcher, error)
	UpdateStatus(*v1alpha1.JanusWatcher) (*v1alpha1.JanusWatcher, error)
	Delete(name string, options *v1.DeleteOptions) error
	DeleteCollection(options *v1.DeleteOptions, listOptions v1.ListOptions) error
	Get(name string, options v1.GetOptions) (*v1alpha1.JanusWatcher, error)
	List(opts v1.ListOptions) (*v1alpha1.JanusWatcherList, error)
	Watch(opts v1.ListOptions) (watch.Interface, error)
	Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1alpha1.JanusWatcher, err error)
	JanusWatcherExpansion
}

// janusWatchers implements JanusWatcherInterface
type janusWatchers struct {
	client rest.Interface
	ns     string
}

// newJanusWatchers returns a JanusWatchers
func newJanusWatchers(c *JanuscontrollerV1alpha1Client, namespace string) *janusWatchers {
	return &janusWatchers{
		client: c.RESTClient(),
		ns:     namespace,
	}
}

// Get takes name of the janusWatcher, and returns the corresponding janusWatcher object, and an error if there is any.
func (c *janusWatchers) Get(name string, options v1.GetOptions) (result *v1alpha1.JanusWatcher, err error) {
	result = &v1alpha1.JanusWatcher{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("januswatchers").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do().
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of JanusWatchers that match those selectors.
func (c *janusWatchers) List(opts v1.ListOptions) (result *v1alpha1.JanusWatcherList, err error) {
	result = &v1alpha1.JanusWatcherList{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("januswatchers").
		VersionedParams(&opts, scheme.ParameterCodec).
		Do().
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested janusWatchers.
func (c *janusWatchers) Watch(opts v1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return c.client.Get().
		Namespace(c.ns).
		Resource("januswatchers").
		VersionedParams(&opts, scheme.ParameterCodec).
		Watch()
}

// Create takes the representation of a janusWatcher and creates it.  Returns the server's representation of the janusWatcher, and an error, if there is any.
func (c *janusWatchers) Create(janusWatcher *v1alpha1.JanusWatcher) (result *v1alpha1.JanusWatcher, err error) {
	result = &v1alpha1.JanusWatcher{}
	err = c.client.Post().
		Namespace(c.ns).
		Resource("januswatchers").
		Body(janusWatcher).
		Do().
		Into(result)
	return
}

// Update takes the representation of a janusWatcher and updates it. Returns the server's representation of the janusWatcher, and an error, if there is any.
func (c *janusWatchers) Update(janusWatcher *v1alpha1.JanusWatcher) (result *v1alpha1.JanusWatcher, err error) {
	result = &v1alpha1.JanusWatcher{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("januswatchers").
		Name(janusWatcher.Name).
		Body(janusWatcher).
		Do().
		Into(result)
	return
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().

func (c *janusWatchers) UpdateStatus(janusWatcher *v1alpha1.JanusWatcher) (result *v1alpha1.JanusWatcher, err error) {
	result = &v1alpha1.JanusWatcher{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("januswatchers").
		Name(janusWatcher.Name).
		SubResource("status").
		Body(janusWatcher).
		Do().
		Into(result)
	return
}

// Delete takes name of the janusWatcher and deletes it. Returns an error if one occurs.
func (c *janusWatchers) Delete(name string, options *v1.DeleteOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("januswatchers").
		Name(name).
		Body(options).
		Do().
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *janusWatchers) DeleteCollection(options *v1.DeleteOptions, listOptions v1.ListOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("januswatchers").
		VersionedParams(&listOptions, scheme.ParameterCodec).
		Body(options).
		Do().
		Error()
}

// Patch applies the patch and returns the patched janusWatcher.
func (c *janusWatchers) Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1alpha1.JanusWatcher, err error) {
	result = &v1alpha1.JanusWatcher{}
	err = c.client.Patch(pt).
		Namespace(c.ns).
		Resource("januswatchers").
		SubResource(subresources...).
		Name(name).
		Body(data).
		Do().
		Into(result)
	return
}
