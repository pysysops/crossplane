/*
Copyright 2020 The Crossplane Authors.

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

package manager

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/test"
	"github.com/crossplane/crossplane/apis/pkg/v1beta1"
)

var _ Revisioner = &MockRevisioner{}

type MockRevisioner struct {
	MockRevision func() (string, error)
}

func NewMockRevisionFn(hash string, err error) func() (string, error) {
	return func() (string, error) {
		return hash, err
	}
}
func (m *MockRevisioner) Revision(context.Context, v1beta1.Package) (string, error) {
	return m.MockRevision()
}

func TestReconcile(t *testing.T) {
	errBoom := errors.New("boom")
	pullAlways := corev1.PullAlways
	trueVal := true
	revHistory := int64(1)

	type args struct {
		req reconcile.Request
		rec *Reconciler
	}
	type want struct {
		r   reconcile.Result
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"PackageNotFound": {
			reason: "We should not return and error and not requeue if package not found.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage: func() v1beta1.Package { return &v1beta1.Configuration{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{MockGet: test.NewMockGetFn(kerrors.NewNotFound(schema.GroupResource{}, ""))},
					},
					log: logging.NewNopLogger(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"ErrGetPackage": {
			reason: "We should return an error if getting package fails.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage: func() v1beta1.Package { return &v1beta1.Configuration{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{MockGet: test.NewMockGetFn(errBoom)},
					},
					log: logging.NewNopLogger(),
				},
			},
			want: want{
				r:   reconcile.Result{},
				err: errors.Wrap(errBoom, errGetPackage),
			},
		},
		"ErrListRevisions": {
			reason: "We should requeue after short wait if listing revisions for a package fails.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet:  test.NewMockGetFn(nil),
							MockList: test.NewMockListFn(errBoom),
						},
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{RequeueAfter: shortWait},
			},
		},
		"SuccessfulNoExistingRevisionsAutoActivate": {
			reason: "We should be active and not requeue on successful creation of the first revision with auto activation.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1beta1.AutomaticActivation)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetActivationPolicy(&v1beta1.AutomaticActivation)
								want.SetConditions(v1beta1.UnknownHealth())
								want.SetConditions(v1beta1.Active())
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ runtime.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"SuccessfulNoExistingRevisionsAutoActivatePullAlways": {
			reason: "We should be active and requeue after wait on successful creation of the first revision with auto activation and package pull policy Always.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1beta1.AutomaticActivation)
								p.SetPackagePullPolicy(&pullAlways)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetActivationPolicy(&v1beta1.AutomaticActivation)
								want.SetPackagePullPolicy(&pullAlways)
								want.SetConditions(v1beta1.UnknownHealth())
								want.SetConditions(v1beta1.Active())
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ runtime.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{RequeueAfter: pullWait},
			},
		},
		"SuccessfulNoExistingRevisionsManualActivate": {
			reason: "We should be inactive and not requeue on successful creation of the first revision with manual activation policy.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1beta1.ManualActivation)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetActivationPolicy(&v1beta1.ManualActivation)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1beta1.UnknownHealth())
								want.SetConditions(v1beta1.Inactive())
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ runtime.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"SuccessfulActiveRevisionExists": {
			reason: "We should match revision health and not requeue when active revision already exists.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o runtime.Object) error {
								l := o.(*v1beta1.ConfigurationRevisionList)
								cr := v1beta1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetConditions(v1beta1.Healthy())
								c := v1beta1.ConfigurationRevisionList{
									Items: []v1beta1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1beta1.Healthy())
								want.SetConditions(v1beta1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ runtime.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"SuccessfulRevisionExistsNeedsActive": {
			reason: "We should match revision health, set to active, and not requeue when inactive revision already exists and activation policy is automatic.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o runtime.Object) error {
								l := o.(*v1beta1.ConfigurationRevisionList)
								cr := v1beta1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1beta1.Healthy())
								cr.SetDesiredState(v1beta1.PackageRevisionInactive)
								cr.SetRevision(1)
								c := v1beta1.ConfigurationRevisionList{
									Items: []v1beta1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1beta1.Healthy())
								want.SetConditions(v1beta1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, o runtime.Object, _ ...resource.ApplyOption) error {
							want := &v1beta1.ConfigurationRevision{}
							want.SetLabels(map[string]string{"pkg.crossplane.io/package": "test"})
							want.SetName("test-1234567")
							want.SetOwnerReferences([]metav1.OwnerReference{{
								APIVersion:         v1beta1.SchemeGroupVersion.String(),
								Kind:               v1beta1.ConfigurationKind,
								Name:               "test",
								Controller:         &trueVal,
								BlockOwnerDeletion: &trueVal,
							}})
							want.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
							want.SetDesiredState(v1beta1.PackageRevisionActive)
							want.SetConditions(v1beta1.Healthy())
							want.SetRevision(1)
							if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
								t.Errorf("-want, +got:\n%s", diff)
							}
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"ErrUpdatePackageRevision": {
			reason: "Failing to update a package revision should cause requeue after short wait.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o runtime.Object) error {
								l := o.(*v1beta1.ConfigurationRevisionList)
								cr := v1beta1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1beta1.Healthy())
								cr.SetDesiredState(v1beta1.PackageRevisionInactive)
								c := v1beta1.ConfigurationRevisionList{
									Items: []v1beta1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1beta1.Healthy())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, o runtime.Object, _ ...resource.ApplyOption) error {
							return errBoom
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{RequeueAfter: shortWait},
			},
		},
		"SuccessfulTransitionUnhealthy": {
			reason: "If the current revision is unhealthy the package should be also.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o runtime.Object) error {
								l := o.(*v1beta1.ConfigurationRevisionList)
								cr := v1beta1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1beta1.Unhealthy())
								cr.SetDesiredState(v1beta1.PackageRevisionActive)
								c := v1beta1.ConfigurationRevisionList{
									Items: []v1beta1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1beta1.Unhealthy())
								want.SetConditions(v1beta1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ runtime.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"SuccessfulRevisionExistsNeedGC": {
			reason: "We should successfully garbage collect when an old revision falls outside range.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o runtime.Object) error {
								l := o.(*v1beta1.ConfigurationRevisionList)
								cr := v1beta1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetRevision(3)
								cr.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1beta1.Healthy())
								cr.SetDesiredState(v1beta1.PackageRevisionInactive)
								c := v1beta1.ConfigurationRevisionList{
									Items: []v1beta1.ConfigurationRevision{
										cr,
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "made-the-cut",
											},
											Spec: v1beta1.PackageRevisionSpec{
												Revision: 2,
											},
										},
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "missed-the-cut",
											},
											Spec: v1beta1.PackageRevisionSpec{
												Revision: 1,
											},
										},
									},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1beta1.Healthy())
								want.SetConditions(v1beta1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
							MockDelete: test.NewMockDeleteFn(nil),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, o runtime.Object, _ ...resource.ApplyOption) error {
							want := &v1beta1.ConfigurationRevision{}
							want.SetLabels(map[string]string{"pkg.crossplane.io/package": "test"})
							want.SetName("test-1234567")
							want.SetOwnerReferences([]metav1.OwnerReference{{
								APIVersion:         v1beta1.SchemeGroupVersion.String(),
								Kind:               v1beta1.ConfigurationKind,
								Name:               "test",
								Controller:         &trueVal,
								BlockOwnerDeletion: &trueVal,
							}})
							want.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
							want.SetDesiredState(v1beta1.PackageRevisionActive)
							want.SetConditions(v1beta1.Healthy())
							want.SetRevision(3)
							if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
								t.Errorf("-want, +got:\n%s", diff)
							}
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{},
			},
		},
		"ErrGC": {
			reason: "Failure to garbage collect old package revision should cause requeue after short wait.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1beta1.Package { return &v1beta1.Configuration{} },
					newPackageRevision:     func() v1beta1.PackageRevision { return &v1beta1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1beta1.PackageRevisionList { return &v1beta1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o runtime.Object) error {
								p := o.(*v1beta1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								p.SetRevisionHistoryLimit(&revHistory)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o runtime.Object) error {
								l := o.(*v1beta1.ConfigurationRevisionList)
								cr := v1beta1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetRevision(3)
								cr.SetGroupVersionKind(v1beta1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1beta1.Healthy())
								cr.SetDesiredState(v1beta1.PackageRevisionInactive)
								c := v1beta1.ConfigurationRevisionList{
									Items: []v1beta1.ConfigurationRevision{
										cr,
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "made-the-cut",
											},
											Spec: v1beta1.PackageRevisionSpec{
												Revision:     2,
												DesiredState: v1beta1.PackageRevisionInactive,
											},
										},
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "missed-the-cut",
											},
											Spec: v1beta1.PackageRevisionSpec{
												Revision:     1,
												DesiredState: v1beta1.PackageRevisionInactive,
											},
										},
									},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockStatusUpdateFn(nil, func(o runtime.Object) error {
								want := &v1beta1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1beta1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetRevisionHistoryLimit(&revHistory)
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
							MockDelete: test.NewMockDeleteFn(errBoom),
						},
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					log:    logging.NewNopLogger(),
					record: event.NewNopRecorder(),
				},
			},
			want: want{
				r: reconcile.Result{RequeueAfter: shortWait},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.args.rec.Reconcile(reconcile.Request{})

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nr.Reconcile(...): -want error, +got error:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.r, got, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nr.Reconcile(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}