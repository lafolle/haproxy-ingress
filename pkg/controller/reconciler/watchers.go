/*
Copyright 2022 The HAProxy Ingress Controller Authors.

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

package reconciler

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/go-logr/logr"
	api "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1alpha1 "sigs.k8s.io/gateway-api/apis/v1alpha1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/jcmoraisjr/haproxy-ingress/pkg/controller/config"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/converters/types"
)

func createWatchers(ctx context.Context, cfg *config.Config) *watchers {
	w := &watchers{
		mu:  sync.Mutex{},
		log: logr.FromContextOrDiscard(ctx).WithName("watchers"),
		cfg: cfg,
	}
	w.initCh()
	return w
}

type watchers struct {
	mu  sync.Mutex
	ch  *types.ChangedObjects
	log logr.Logger
	cfg *config.Config
	run bool
}

func (w *watchers) getHandlers() []*hdlr {
	handlers := w.handlersCore()
	handlers = append(handlers, w.handlersIngress()...)
	if w.cfg.HasGatewayA1 {
		handlers = append(handlers, w.handlersGatewayv1alpha1()...)
	}
	if w.cfg.HasGateway {
		handlers = append(handlers, w.handlersGatewayv1alpha2()...)
	}
	for _, h := range handlers {
		h.w = w
	}
	return handlers
}

func (w *watchers) getChangedObjects() *types.ChangedObjects {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := *w.ch
	w.initCh()
	w.run = true
	return &ch
}

func (w *watchers) initCh() {
	newch := new(types.ChangedObjects)
	if w.ch != nil {
		if w.ch.GlobalConfigMapDataNew != nil {
			newch.GlobalConfigMapDataCur = w.ch.GlobalConfigMapDataNew
		} else {
			newch.GlobalConfigMapDataCur = w.ch.GlobalConfigMapDataCur
		}
		if w.ch.TCPConfigMapDataNew != nil {
			newch.TCPConfigMapDataCur = w.ch.TCPConfigMapDataNew
		} else {
			newch.TCPConfigMapDataCur = w.ch.TCPConfigMapDataCur
		}
	}
	w.ch = newch
	w.ch.Links = types.TrackingLinks{}
}

func (w *watchers) handlersCore() []*hdlr {
	cmChange := func(o client.Object) {
		cm := o.(*api.ConfigMap)
		key := cm.Namespace + "/" + cm.Name
		switch key {
		case w.cfg.ConfigMapName:
			w.ch.GlobalConfigMapDataNew = cm.Data
		case w.cfg.TCPConfigMapName:
			w.ch.TCPConfigMapDataNew = cm.Data
		}
	}
	return []*hdlr{
		{
			typ: &api.ConfigMap{},
			res: types.ResourceConfigMap,
			add: cmChange,
			upd: cmChange,
			pr: []predicate.Predicate{
				predicate.NewPredicateFuncs(func(o client.Object) bool {
					cm := o.(*api.ConfigMap)
					key := cm.Namespace + "/" + cm.Name
					return key == w.cfg.ConfigMapName || key == w.cfg.TCPConfigMapName
				}),
			},
		},
		{
			typ: &api.Service{},
			res: types.ResourceService,
			pr: []predicate.Predicate{predicate.Or(
				predicate.AnnotationChangedPredicate{},
				predicate.GenerationChangedPredicate{},
			)},
		},
		{
			typ: &api.Endpoints{},
			res: types.ResourceEndpoints,
			pr: []predicate.Predicate{
				predicate.GenerationChangedPredicate{},
			},
		},
		{
			typ: &api.Secret{},
			res: types.ResourceSecret,
		},
		{
			typ: &api.Pod{},
			res: types.ResourcePod,
			pr: []predicate.Predicate{
				predicate.Funcs{
					CreateFunc: func(e event.CreateEvent) bool { return false },
					UpdateFunc: func(e event.UpdateEvent) bool {
						if e.ObjectOld == nil || e.ObjectNew == nil {
							return true
						}
						old := e.ObjectOld.(*api.Pod)
						new := e.ObjectNew.(*api.Pod)
						return old.DeletionTimestamp != new.DeletionTimestamp
					},
				},
			},
		},
	}
}

func (w *watchers) handlersIngress() []*hdlr {
	return []*hdlr{
		{
			typ: &networking.Ingress{},
			res: types.ResourceIngress,
			add: func(o client.Object) {
				w.ch.IngressesAdd = append(w.ch.IngressesAdd, o.(*networking.Ingress))
			},
			upd: func(o client.Object) {
				w.ch.IngressesUpd = append(w.ch.IngressesUpd, o.(*networking.Ingress))
			},
			del: func(o client.Object) {
				w.ch.IngressesDel = append(w.ch.IngressesUpd, o.(*networking.Ingress))
			},
			// TODO: apply rules from legacy lister
			pr: []predicate.Predicate{predicate.Or(
				predicate.AnnotationChangedPredicate{},
				predicate.GenerationChangedPredicate{},
			)},
		},
		{
			typ: &networking.IngressClass{},
			res: types.ResourceIngressClass,
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
	}
}

func (w *watchers) handlersGatewayv1alpha1() []*hdlr {
	return []*hdlr{
		{
			typ: &gatewayv1alpha1.Gateway{},
			res: types.ResourceGatewayA1,
			add: func(o client.Object) {
				w.ch.GatewaysA1Add = append(w.ch.GatewaysA1Add, o.(*gatewayv1alpha1.Gateway))
			},
			upd: func(o client.Object) {
				w.ch.GatewaysA1Upd = append(w.ch.GatewaysA1Upd, o.(*gatewayv1alpha1.Gateway))
			},
			del: func(o client.Object) {
				w.ch.GatewaysA1Del = append(w.ch.GatewaysA1Del, o.(*gatewayv1alpha1.Gateway))
			},
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
		{
			typ: &gatewayv1alpha1.GatewayClass{},
			res: types.ResourceGatewayClassA1,
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
		{
			typ: &gatewayv1alpha1.HTTPRoute{},
			res: types.ResourceHTTPRouteA1,
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
	}
}

func (w *watchers) handlersGatewayv1alpha2() []*hdlr {
	return []*hdlr{
		{
			typ: &gatewayv1alpha2.Gateway{},
			res: types.ResourceGateway,
			add: func(o client.Object) {
				w.ch.GatewaysAdd = append(w.ch.GatewaysAdd, o.(*gatewayv1alpha2.Gateway))
			},
			upd: func(o client.Object) {
				w.ch.GatewaysUpd = append(w.ch.GatewaysUpd, o.(*gatewayv1alpha2.Gateway))
			},
			del: func(o client.Object) {
				w.ch.GatewaysDel = append(w.ch.GatewaysDel, o.(*gatewayv1alpha2.Gateway))
			},
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
		{
			typ: &gatewayv1alpha2.GatewayClass{},
			res: types.ResourceGatewayClass,
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
		{
			typ: &gatewayv1alpha2.HTTPRoute{},
			res: types.ResourceHTTPRoute,
			pr: []predicate.Predicate{predicate.Or(
				predicate.GenerationChangedPredicate{},
			)},
		},
	}
}

type hdlr struct {
	w   *watchers
	typ client.Object
	res types.ResourceType
	pr  []predicate.Predicate
	add,
	upd,
	del func(o client.Object)
}

func (h *hdlr) getSource() source.Source {
	return &source.Kind{Type: h.typ}
}

func (h *hdlr) getEventHandler() handler.EventHandler {
	return h
}

func (h *hdlr) getPredicates() []predicate.Predicate {
	return h.pr
}

func (h *hdlr) Create(e event.CreateEvent, q workqueue.RateLimitingInterface) {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if e.Object != nil {
		if h.add != nil {
			h.add(e.Object)
		}
		h.compose("add", e.Object)
	} else {
		h.generic()
	}
	h.notify(e.Object, q)
}

func (h *hdlr) Update(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if e.ObjectOld != nil && e.ObjectNew != nil {
		if h.upd != nil {
			h.upd(e.ObjectNew)
		}
		h.compose("update", e.ObjectNew)
	} else {
		h.generic()
	}
	h.notify(e.ObjectNew, q)
}

func (h *hdlr) Delete(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if e.Object != nil {
		if h.del != nil {
			h.del(e.Object)
		}
		h.compose("del", e.Object)
	} else {
		h.generic()
	}
	h.notify(e.Object, q)
}

func (h *hdlr) Generic(e event.GenericEvent, q workqueue.RateLimitingInterface) {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	h.generic()
	h.notify(e.Object, q)
}

func (h *hdlr) generic() {
	h.w.ch.NeedFullSync = true
}

func (h *hdlr) compose(ev string, obj client.Object) {
	ns := obj.GetNamespace()
	fullname := obj.GetName()
	if ns != "" {
		fullname = ns + "/" + fullname
	}
	ch := h.w.ch
	ch.Links[h.res] = append(ch.Links[h.res], fullname)
	ch.Objects = append(ch.Objects, fmt.Sprintf("%s/%s:%s", ev, h.res, fullname))
}

func (h *hdlr) notify(o client.Object, q workqueue.RateLimitingInterface) {
	q.AddRateLimited(reconcile.Request{})
	if o != nil && h.w.run {
		h.w.log.Info("notify", "kind", reflect.TypeOf(o), "namespace", o.GetNamespace(), "name", o.GetName())
	}
}
