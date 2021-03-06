/*
Copyright 2018 Google LLC
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

package genericreconciler

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	urt "k8s.io/apimachinery/pkg/util/runtime"
	"log"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	app "sigs.k8s.io/kubesdk/pkg/application"
	"sigs.k8s.io/kubesdk/pkg/component"
	cr "sigs.k8s.io/kubesdk/pkg/customresource"
	"sigs.k8s.io/kubesdk/pkg/resource"
)

func handleErrorArr(info string, name string, e error, errs []error) []error {
	HandleError(info, name, e)
	return append(errs, e)
}

// HandleError common error handling routine
func HandleError(info string, name string, e error) error {
	urt.HandleError(fmt.Errorf("Failed: [%s] %s. %s", name, info, e.Error()))
	return e
}

func (gr *Reconciler) observe(observables ...resource.Observable) (*resource.ObjectBag, error) {
	var returnval *resource.ObjectBag = new(resource.ObjectBag)
	var err error
	for _, obs := range observables {
		var resources []resource.Object
		if obs.Labels != nil {
			//log.Printf("   >>>list: %s labels:[%v]", reflect.TypeOf(obs.ObjList).String(), obs.Labels)
			opts := client.MatchingLabels(obs.Labels)
			opts.Raw = &metav1.ListOptions{TypeMeta: obs.Type}
			err = gr.List(context.TODO(), opts, obs.ObjList.(runtime.Object))
			if err == nil {
				items, err := meta.ExtractList(obs.ObjList.(runtime.Object))
				if err == nil {
					for _, item := range items {
						resources = append(resources, resource.Object{Obj: item.(metav1.Object)})
					}
				}
				/*
					//items := reflect.Indirect(reflect.ValueOf(obs.ObjList)).FieldByName("Items")
					for i := 0; i < items.Len(); i++ {
						o := items.Index(i)
						resources = append(resources, resource.Object{Obj: o.Addr().Interface().(metav1.Object)})
					}
				*/
			}
		} else {
			// check typecasting ?
			// TODO check obj := obs.Obj.(metav1.Object)
			var obj metav1.Object = obs.Obj.(metav1.Object)
			name := obj.GetName()
			namespace := obj.GetNamespace()
			otype := reflect.TypeOf(obj).String()
			err = gr.Get(context.TODO(),
				types.NamespacedName{Name: name, Namespace: namespace},
				obs.Obj.(runtime.Object))
			if err == nil {
				log.Printf("   >>get: %s", otype+"/"+namespace+"/"+name)
				resources = append(resources, resource.Object{Obj: obs.Obj})
			} else {
				log.Printf("   >>>ERR get: %s", otype+"/"+namespace+"/"+name)
			}
		}
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			returnval.Add(resource)
		}
	}
	return returnval, nil
}

func specDiffers(o1, o2 metav1.Object) bool {
	// Not all k8s objects have Spec
	// example ConfigMap
	// TODO strategic merge patch diff in generic controller loop
	e := reflect.Indirect(reflect.ValueOf(o1)).FieldByName("Spec")
	o := reflect.Indirect(reflect.ValueOf(o2)).FieldByName("Spec")
	if !e.IsValid() {
		// handling ConfigMap
		e = reflect.Indirect(reflect.ValueOf(o1)).FieldByName("Data")
		o = reflect.Indirect(reflect.ValueOf(o2)).FieldByName("Data")
	}
	if e.IsValid() && o.IsValid() {
		if reflect.DeepEqual(e.Interface(), o.Interface()) {
			return false
		}
	}
	return true
}

// ReconcileCR is a generic function that reconciles expected and observed resources
func (gr *Reconciler) ReconcileCR(namespacedname types.NamespacedName, handle cr.Handle) error {
	var status interface{}
	expected := &resource.ObjectBag{}
	update := false
	rsrc := handle.NewRsrc()
	name := reflect.TypeOf(rsrc).String() + "/" + namespacedname.String()
	err := gr.Get(context.TODO(), namespacedname, rsrc.(runtime.Object))
	if err == nil {
		o := rsrc.(metav1.Object)
		log.Printf("%s Validating spec\n", name)
		err = rsrc.Validate()
		status = rsrc.NewStatus()
		if err == nil {
			log.Printf("%s Applying defaults\n", name)
			rsrc.ApplyDefaults()
			components := rsrc.Components()
			for _, component := range components {
				if o.GetDeletionTimestamp() == nil {
					err = gr.ReconcileComponent(name, component, status, expected)
				} else {
					err = gr.FinalizeComponent(name, component, status, expected)
				}
			}
		}
	} else {
		if errors.IsNotFound(err) {
			urt.HandleError(fmt.Errorf("not found %s. %s", name, err.Error()))
			// TODO check if we need to return err for not found err
			return err
		}
	}
	update = rsrc.UpdateRsrcStatus(status, err)

	if update {
		err = gr.Update(context.TODO(), rsrc.(runtime.Object))
	}
	if err != nil {
		urt.HandleError(fmt.Errorf("error updating %s. %s", name, err.Error()))
	}

	return err
}

// ObserveAndMutate is a function that is called to observe and mutate expected resources
func (gr *Reconciler) ObserveAndMutate(crname string, c component.Component, status interface{}, mutate bool, aggregated *resource.ObjectBag) (*resource.ObjectBag, *resource.ObjectBag, string, error) {
	var err error
	var expected, observed *resource.ObjectBag

	// Get Expected resources
	stage := "gathering expected resources"
	expected, err = c.ExpectedResources(c.CR, c.Labels(), aggregated)
	if err == nil && expected != nil {
		// Get observables
		observables := c.Observables(gr.Scheme, c.CR, c.Labels(), expected)
		// Observe observables
		stage = "observing resources"
		observed, err = gr.observe(observables...)
		if mutate && err == nil {
			// Mutate expected objects
			stage = "mutating resources"
			expected, err = c.Mutate(c.CR, status, expected, observed)
		}
	}
	if err != nil {
		observed = &resource.ObjectBag{}
		expected = &resource.ObjectBag{}
	}
	if expected == nil {
		expected = &resource.ObjectBag{}
	}
	if observed == nil {
		observed = &resource.ObjectBag{}
	}
	return expected, observed, stage, err
}

// FinalizeComponent is a function that finalizes component
func (gr *Reconciler) FinalizeComponent(crname string, c component.Component, status interface{}, aggregated *resource.ObjectBag) error {
	cname := crname + "(cmpnt:" + c.Name + ")"
	log.Printf("%s  { finalizing component\n", cname)
	defer log.Printf("%s  } finalizing component\n", cname)

	expected, observed, stage, err := gr.ObserveAndMutate(crname, c, status, false, aggregated)

	if err != nil {
		HandleError(stage, crname, err)
	}
	aggregated.Add(expected.Items()...)
	err = c.Finalize(c.CR, status, observed)
	return err
}

// ReconcileComponent is a generic function that reconciles expected and observed resources
func (gr *Reconciler) ReconcileComponent(crname string, c component.Component, status interface{}, aggregated *resource.ObjectBag) error {
	errs := []error{}
	reconciled := []metav1.Object{}

	cname := crname + "(cmpnt:" + c.Name + ")"
	log.Printf("%s  { reconciling component\n", cname)
	defer log.Printf("%s  } reconciling component\n", cname)

	expected, observed, stage, err := gr.ObserveAndMutate(crname, c, status, true, aggregated)

	// Reconciliation logic is straight-forward:
	// This method gets the list of expected resources and observed resources
	// We compare the 2 lists and:
	//  create(rsrc) where rsrc is in expected but not in observed
	//  delete(rsrc) where rsrc is in observed but not in expected
	//  update(rsrc) where rsrc is in observed and expected
	//
	// We have a notion of Managed and Referred resources
	// Only Managed resources are CRUD'd
	// Missing Reffered resources are treated as errors and surfaced as such in the status field
	//

	if err != nil {
		errs = handleErrorArr(stage, crname, err, errs)
	} else {
		aggregated.Add(expected.Items()...)
		log.Printf("%s  Expected Resources:\n", cname)
		for _, e := range expected.Items() {
			e.Obj.SetOwnerReferences(c.OwnerRef)
			log.Printf("%s   exp: %s/%s/%s\n", cname, e.Obj.GetNamespace(), reflect.TypeOf(e.Obj).String(), e.Obj.GetName())
		}
		log.Printf("%s  Observed Resources:\n", cname)
		for _, e := range observed.Items() {
			log.Printf("%s   obs: %s/%s/%s\n", cname, e.Obj.GetNamespace(), reflect.TypeOf(e.Obj).String(), e.Obj.GetName())
		}

		log.Printf("%s  Reconciling Resources:\n", cname)
	}
	for _, e := range expected.Items() {
		seen := false
		eNamespace := e.Obj.GetNamespace()
		eName := e.Obj.GetName()
		eKind := reflect.TypeOf(e.Obj).String()
		eRsrcInfo := eNamespace + "/" + eKind + "/" + eName
		for _, o := range observed.Items() {
			if (eName == o.Obj.GetName()) &&
				(eNamespace == o.Obj.GetNamespace()) &&
				(eKind == reflect.TypeOf(o.Obj).String()) {
				// rsrc is seen in both expected and observed, update it if needed
				e.Obj.SetResourceVersion(o.Obj.GetResourceVersion())
				if e.Lifecycle == resource.LifecycleManaged && specDiffers(e.Obj, o.Obj) && c.Differs(e.Obj, o.Obj) {
					if err := gr.Update(context.TODO(), e.Obj.(runtime.Object).DeepCopyObject()); err != nil {
						errs = handleErrorArr("update", eRsrcInfo, err, errs)
					} else {
						log.Printf("%s   update: %s\n", cname, eRsrcInfo)
					}
				} else {
					log.Printf("%s   nochange: %s\n", cname, eRsrcInfo)
				}
				reconciled = append(reconciled, o.Obj)
				seen = true
				break
			}
		}
		// rsrc is in expected but not in observed - create
		if !seen {
			if e.Lifecycle == resource.LifecycleManaged {
				if err := gr.Create(context.TODO(), e.Obj.(runtime.Object)); err != nil {
					errs = handleErrorArr("Create", cname, err, errs)
				} else {
					log.Printf("%s   +create: %s\n", cname, eRsrcInfo)
					reconciled = append(reconciled, e.Obj)
				}
			} else {
				err := fmt.Errorf("missing resource not managed by %s: %s", cname, eRsrcInfo)
				errs = handleErrorArr("missing resource", cname, err, errs)
			}
		}
	}

	// delete(observed - expected)
	for _, o := range observed.Items() {
		seen := false
		oNamespace := o.Obj.GetNamespace()
		oName := o.Obj.GetName()
		oKind := reflect.TypeOf(o.Obj).String()
		oRsrcInfo := oKind + "/" + oNamespace + "/" + oName
		for _, e := range expected.Items() {
			if (e.Obj.GetName() == oName) &&
				(e.Obj.GetNamespace() == oNamespace) &&
				(reflect.TypeOf(o.Obj).String() == oKind) {
				seen = true
				break
			}
		}
		// rsrc is in observed but not in expected - delete
		if !seen {
			if err := gr.Delete(context.TODO(), o.Obj.(runtime.Object)); err != nil {
				errs = handleErrorArr("delete", oRsrcInfo, err, errs)
			} else {
				log.Printf("%s   -delete: %s\n", cname, oRsrcInfo)
			}
		}
	}

	err = utilerrors.NewAggregate(errs)
	c.UpdateComponentStatus(c.CR, status, reconciled, err)
	return err
}

// Reconcile expected by kubebuilder
func (gr *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	err := gr.ReconcileCR(request.NamespacedName, gr.Handle)
	if err != nil {
		fmt.Printf("err: %s", err.Error())
	}
	return reconcile.Result{}, err
}

// AddToSchemes for adding Application to scheme
var AddToSchemes runtime.SchemeBuilder

// Init sets up Reconciler
func (gr *Reconciler) Init() {
	gr.Client = gr.Manager.GetClient()
	gr.Scheme = gr.Manager.GetScheme()
	app.AddToScheme(&AddToSchemes)
	AddToSchemes.AddToScheme(gr.Scheme)
}
