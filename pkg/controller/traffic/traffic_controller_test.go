package traffic

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	shipperv1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	shipperfake "github.com/bookingcom/shipper/pkg/client/clientset/versioned/fake"
	shipperinformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	"github.com/bookingcom/shipper/pkg/clusterclientstore"
	"github.com/bookingcom/shipper/pkg/conditions"
	shippertesting "github.com/bookingcom/shipper/pkg/testing"
)

const (
	trafficLabel = "gets-the-traffic"
	trafficValue = "you-betcha"
)

func init() {
	conditions.TrafficConditionsShouldDiscardTimestamps = true
}

func TestSingleCluster(t *testing.T) {
	f := newFixture(t)
	app := "test-app"
	release := "test-app-1234"
	cluster := f.newCluster()
	cluster.AddOne(buildService(app))

	const noTraffic = false
	pods := buildPods(app, release, 1, noTraffic)
	cluster.AddMany(pods)

	tt := buildTrafficTarget(
		app, release,
		map[string]uint32{
			cluster.Name: 10,
		},
	)

	f.addTrafficTarget(tt)
	updatedTT := tt.DeepCopy()
	updatedTT.Status.Clusters = buildTotalSuccessStatus(updatedTT)

	pod := pods[0].(*corev1.Pod)
	updatedPod := pod.DeepCopy()
	gvr := corev1.SchemeGroupVersion.WithResource("pods")
	updatedPod.GetLabels()[trafficLabel] = trafficValue
	cluster.Expect(kubetesting.NewUpdateAction(gvr, shippertesting.TestNamespace, updatedPod))

	f.expectTrafficTargetUpdate(updatedTT)
	f.run()
}

func TestExtraClustersNoExtraStatuses(t *testing.T) {
	f := newFixture(t)
	app := "test-app"
	releaseA := "test-app-1234"
	releaseB := "test-app-4567"

	clusterA := f.newCluster()
	clusterB := f.newCluster()

	clusterA.AddOne(buildService(app))
	clusterB.AddOne(buildService(app))

	const withTraffic = true
	podsA := buildPods(app, releaseA, 1, withTraffic)
	clusterA.AddMany(podsA)

	podsB := buildPods(app, releaseB, 1, withTraffic)
	clusterB.AddMany(podsB)

	ttA := buildTrafficTarget(
		app, releaseA,
		map[string]uint32{
			clusterA.Name: 10,
		},
	)

	ttB := buildTrafficTarget(
		app, releaseB,
		map[string]uint32{
			clusterB.Name: 10,
		},
	)

	f.addTrafficTarget(ttA)
	f.addTrafficTarget(ttB)

	updatedA := ttA.DeepCopy()
	updatedA.Status.Clusters = buildTotalSuccessStatus(updatedA)

	updatedB := ttB.DeepCopy()
	updatedB.Status.Clusters = buildTotalSuccessStatus(updatedB)

	f.expectTrafficTargetUpdate(updatedA)
	f.expectTrafficTargetUpdate(updatedB)
	f.run()
}

type fixture struct {
	t *testing.T

	trafficTargetCount int

	objects []runtime.Object
	actions []kubetesting.Action

	clusters []*shippertesting.ClusterFixture
}

func newFixture(t *testing.T) *fixture {
	return &fixture{
		t: t,
	}
}

func (f *fixture) newCluster() *shippertesting.ClusterFixture {
	name := fmt.Sprintf("cluster-%d", len(f.clusters))
	cluster := shippertesting.NewClusterFixture(name)
	f.clusters = append(f.clusters, cluster)
	return cluster
}

func (f *fixture) newController(
	stopCh chan struct{},
) (
	*shipperfake.Clientset,
	*Controller,
	*clusterclientstore.Store,
	shipperinformers.SharedInformerFactory,
) {

	client := shipperfake.NewSimpleClientset(f.objects...)

	clusterNames := make([]string, 0, len(f.clusters))
	for _, cluster := range f.clusters {
		clusterNames = append(clusterNames, cluster.Name)
	}

	store := shippertesting.ClusterClientStore(
		stopCh,
		clusterNames,
		func(clusterName string, _ *rest.Config) (kubernetes.Interface, error) {
			for _, cluster := range f.clusters {
				if clusterName == cluster.Name {
					return kubefake.NewSimpleClientset(cluster.Objects()...), nil
				}
			}
			f.t.Fatalf("tried to build a client for a cluster %q which was not present in the test fixture. this is a bug in the tests", clusterName)
			return nil, fmt.Errorf("no such cluster")
		},
	)

	shipperInformerFactory := shipperinformers.NewSharedInformerFactory(client, shippertesting.NoResyncPeriod)
	c := NewController(
		client, shipperInformerFactory, store, record.NewFakeRecorder(42),
	)

	return client, c, store, shipperInformerFactory
}

func (f *fixture) run() {
	stopCh := make(chan struct{})
	defer close(stopCh)

	client, controller, store, informer := f.newController(stopCh)

	runtimeutil.ErrorHandlers = []func(error){
		func(err error) {
			f.t.Errorf("runtime.Error invoked: %q", err)
		},
	}

	go store.Run(stopCh)

	wait.PollUntil(
		10*time.Millisecond,
		func() (bool, error) {
			// poll until the clusters are prepared in the cluster store
			for _, cluster := range f.clusters {
				_, err := store.GetClient(cluster.Name)
				if err != nil {
					return false, nil
				}
			}
			return true, nil
		},
		stopCh,
	)

	informer.Start(stopCh)
	informer.WaitForCacheSync(stopCh)

	wait.PollUntil(
		10*time.Millisecond,
		func() (bool, error) { return controller.workqueue.Len() >= f.trafficTargetCount, nil },
		stopCh,
	)

	for i := 0; i < f.trafficTargetCount; i++ {
		controller.processNextWorkItem()
	}

	actual := shippertesting.FilterActions(client.Actions())
	shippertesting.CheckActions(f.actions, actual, f.t)

	shippertesting.CheckClusterClientActions(store, f.clusters, f.t)
}

func (f *fixture) addTrafficTarget(tt *shipperv1.TrafficTarget) {
	f.trafficTargetCount++
	f.objects = append(f.objects, tt)
}

func (f *fixture) expectTrafficTargetUpdate(tt *shipperv1.TrafficTarget) {
	gvr := shipperv1.SchemeGroupVersion.WithResource("traffictargets")
	action := kubetesting.NewUpdateAction(gvr, tt.GetNamespace(), tt)
	f.actions = append(f.actions, action)
}

func buildTrafficTarget(app, release string, clusterWeights map[string]uint32) *shipperv1.TrafficTarget {
	clusters := make([]shipperv1.ClusterTrafficTarget, 0, len(clusterWeights))

	for cluster, weight := range clusterWeights {
		clusters = append(clusters, shipperv1.ClusterTrafficTarget{
			Name:   cluster,
			Weight: weight,
		})
	}

	return &shipperv1.TrafficTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release,
			Namespace: shippertesting.TestNamespace,
			Labels: map[string]string{
				shipperv1.AppLabel:     app,
				shipperv1.ReleaseLabel: release,
			},
		},
		Spec: shipperv1.TrafficTargetSpec{
			Clusters: clusters,
		},
	}
}

func buildTotalSuccessStatus(tt *shipperv1.TrafficTarget) []*shipperv1.ClusterTrafficStatus {
	clusterStatuses := make([]*shipperv1.ClusterTrafficStatus, 0, len(tt.Spec.Clusters))

	for _, cluster := range tt.Spec.Clusters {
		clusterStatuses = append(clusterStatuses, &shipperv1.ClusterTrafficStatus{
			Name:            cluster.Name,
			AchievedTraffic: cluster.Weight,
			Status:          "Synced",
			Conditions: []shipperv1.ClusterTrafficCondition{
				shipperv1.ClusterTrafficCondition{
					Type:   shipperv1.ClusterConditionTypeOperational,
					Status: corev1.ConditionTrue,
				},
				shipperv1.ClusterTrafficCondition{
					Type:   shipperv1.ClusterConditionTypeReady,
					Status: corev1.ConditionTrue,
				},
			},
		})
	}

	return clusterStatuses
}

func buildService(app string) runtime.Object {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-prod", app),
			Namespace: shippertesting.TestNamespace,
			Labels: map[string]string{
				shipperv1.LBLabel:  shipperv1.LBForProduction,
				shipperv1.AppLabel: app,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				trafficLabel: trafficValue,
			},
		},
	}
}

func buildPods(app, release string, count int, withTraffic bool) []runtime.Object {
	pods := make([]runtime.Object, 0, count)
	for i := 0; i < count; i++ {
		getsTraffic := trafficValue
		if !withTraffic {
			getsTraffic = "not-a-chance"
		}
		pods = append(pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", release, i),
				Namespace: shippertesting.TestNamespace,
				Labels: map[string]string{
					trafficLabel:           getsTraffic,
					shipperv1.AppLabel:     app,
					shipperv1.ReleaseLabel: release,
				},
			},
		})
	}
	return pods
}
