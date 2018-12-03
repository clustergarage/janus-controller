// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	internalinterfaces "clustergarage.io/janus-controller/pkg/client/informers/externalversions/internalinterfaces"
)

// Interface provides access to all the informers in this group version.
type Interface interface {
	// JanusWatchers returns a JanusWatcherInformer.
	JanusWatchers() JanusWatcherInformer
}

type version struct {
	factory          internalinterfaces.SharedInformerFactory
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
	return &version{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// JanusWatchers returns a JanusWatcherInformer.
func (v *version) JanusWatchers() JanusWatcherInformer {
	return &janusWatcherInformer{factory: v.factory, namespace: v.namespace, tweakListOptions: v.tweakListOptions}
}