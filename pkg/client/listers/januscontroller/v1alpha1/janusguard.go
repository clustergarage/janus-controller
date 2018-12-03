// Code generated by lister-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// JanusGuardLister helps list JanusGuards.
type JanusGuardLister interface {
	// List lists all JanusGuards in the indexer.
	List(selector labels.Selector) (ret []*v1alpha1.JanusGuard, err error)
	// JanusGuards returns an object that can list and get JanusGuards.
	JanusGuards(namespace string) JanusGuardNamespaceLister
	JanusGuardListerExpansion
}

// janusGuardLister implements the JanusGuardLister interface.
type janusGuardLister struct {
	indexer cache.Indexer
}

// NewJanusGuardLister returns a new JanusGuardLister.
func NewJanusGuardLister(indexer cache.Indexer) JanusGuardLister {
	return &janusGuardLister{indexer: indexer}
}

// List lists all JanusGuards in the indexer.
func (s *janusGuardLister) List(selector labels.Selector) (ret []*v1alpha1.JanusGuard, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha1.JanusGuard))
	})
	return ret, err
}

// JanusGuards returns an object that can list and get JanusGuards.
func (s *janusGuardLister) JanusGuards(namespace string) JanusGuardNamespaceLister {
	return janusGuardNamespaceLister{indexer: s.indexer, namespace: namespace}
}

// JanusGuardNamespaceLister helps list and get JanusGuards.
type JanusGuardNamespaceLister interface {
	// List lists all JanusGuards in the indexer for a given namespace.
	List(selector labels.Selector) (ret []*v1alpha1.JanusGuard, err error)
	// Get retrieves the JanusGuard from the indexer for a given namespace and name.
	Get(name string) (*v1alpha1.JanusGuard, error)
	JanusGuardNamespaceListerExpansion
}

// janusGuardNamespaceLister implements the JanusGuardNamespaceLister
// interface.
type janusGuardNamespaceLister struct {
	indexer   cache.Indexer
	namespace string
}

// List lists all JanusGuards in the indexer for a given namespace.
func (s janusGuardNamespaceLister) List(selector labels.Selector) (ret []*v1alpha1.JanusGuard, err error) {
	err = cache.ListAllByNamespace(s.indexer, s.namespace, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha1.JanusGuard))
	})
	return ret, err
}

// Get retrieves the JanusGuard from the indexer for a given namespace and name.
func (s janusGuardNamespaceLister) Get(name string) (*v1alpha1.JanusGuard, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1alpha1.Resource("janusguard"), name)
	}
	return obj.(*v1alpha1.JanusGuard), nil
}
