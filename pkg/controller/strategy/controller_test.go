package strategy

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	shipperV1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	shipperfake "github.com/bookingcom/shipper/pkg/client/clientset/versioned/fake"
	shipperinformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	"github.com/bookingcom/shipper/pkg/conditions"
	shippertesting "github.com/bookingcom/shipper/pkg/testing"
	releaseutil "github.com/bookingcom/shipper/pkg/util/release"
)

func init() {
	releaseutil.ConditionsShouldDiscardTimestamps = true
	conditions.StrategyConditionsShouldDiscardTimestamps = true
}

func TestContenderReleasePhaseIsWaitingForCommandForInitialStepState(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	// Strategy specifies that step 0 the contender has a minimum number of pods
	// (1), no traffic yet.
	contender.capacityTarget.Spec.Clusters[0].Percent = 1

	f.addObjects(contender, incumbent)

	rel := contender.release.DeepCopy()
	f.expectReleaseWaitingForCommand(rel, 0)
	f.run()
}

func TestContenderDoNothingClusterInstallationNotReady(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	addCluster(contender, "broken-installation-cluster")

	contender.release.Spec.TargetStep = 0

	// The fixture creates installation targets in 'installation succeeded' status,
	// so we'll break one.
	contender.installationTarget.Status.Clusters[1].Status = shipperV1.InstallationStatusFailed

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectInstallationNotReady(r, nil, 0, Contender)
	f.run()
}

func TestContenderDoNothingClusterCapacityNotReady(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	brokenClusterName := "broken-capacity-cluster"
	addCluster(contender, brokenClusterName)

	// We'll set cluster 0 to be all set, but make cluster 1 broken.
	contender.release.Spec.TargetStep = 1
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	contender.capacityTarget.Spec.Clusters[1].Percent = 50
	// No capacity yet.
	contender.capacityTarget.Status.Clusters[1].AchievedPercent = 0

	contender.trafficTarget.Spec.Clusters[1].Weight = 50
	contender.trafficTarget.Status.Clusters[1].AchievedTraffic = 50

	incumbent.trafficTarget.Spec.Clusters[0].Weight = 50
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 50
	incumbent.capacityTarget.Spec.Clusters[0].Percent = 50
	incumbent.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectCapacityNotReady(r, 1, 0, Contender, brokenClusterName)
	f.run()
}

func TestContenderDoNothingClusterTrafficNotReady(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	brokenClusterName := "broken-traffic-cluster"
	addCluster(contender, brokenClusterName)
	// We'll set cluster 0 to be all set, but make cluster 1 broken.
	contender.release.Spec.TargetStep = 1
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	contender.capacityTarget.Spec.Clusters[1].Percent = 50
	contender.capacityTarget.Status.Clusters[1].AchievedPercent = 50

	contender.trafficTarget.Spec.Clusters[1].Weight = 50
	// No traffic yet.
	contender.trafficTarget.Status.Clusters[1].AchievedTraffic = 0

	incumbent.trafficTarget.Spec.Clusters[0].Weight = 50
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 50
	incumbent.capacityTarget.Spec.Clusters[0].Percent = 50
	incumbent.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectTrafficNotReady(r, 1, 0, Contender, brokenClusterName)
	f.run()
}

func TestContenderCapacityShouldIncrease(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1

	f.addObjects(contender, incumbent)

	ct := contender.capacityTarget.DeepCopy()
	r := contender.release.DeepCopy()
	f.expectCapacityStatusPatch(ct, r, 50, totalReplicaCount, Contender)
	f.run()
}

type role int

const (
	Contender = iota
	Incumbent
)

func TestContenderTrafficShouldIncrease(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	f.addObjects(contender, incumbent)

	tt := contender.trafficTarget.DeepCopy()
	r := contender.release.DeepCopy()
	f.expectTrafficStatusPatch(tt, r, 50, Contender)
	f.run()
}

func TestIncumbentTrafficShouldDecrease(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	f.addObjects(contender, incumbent)

	tt := incumbent.trafficTarget.DeepCopy()
	r := contender.release.DeepCopy()
	f.expectTrafficStatusPatch(tt, r, 50, Incumbent)
	f.run()
}

func TestIncumbentCapacityShouldDecrease(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50
	incumbent.trafficTarget.Spec.Clusters[0].Weight = 50
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	f.addObjects(contender, incumbent)

	tt := incumbent.capacityTarget.DeepCopy()
	r := contender.release.DeepCopy()
	f.expectCapacityStatusPatch(tt, r, 50, totalReplicaCount, Incumbent)
	f.run()
}

func TestContenderReleasePhaseIsWaitingForCommandForFinalStepState(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50
	incumbent.trafficTarget.Spec.Clusters[0].Weight = 50
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 50
	incumbent.capacityTarget.Spec.Clusters[0].Percent = 50
	incumbent.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	f.addObjects(contender, incumbent)

	rel := contender.release.DeepCopy()
	f.expectReleaseWaitingForCommand(rel, 1)
	f.run()
}

func TestContenderReleaseIsInstalled(t *testing.T) {
	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 2
	contender.capacityTarget.Spec.Clusters[0].Percent = 100
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 100
	contender.trafficTarget.Spec.Clusters[0].Weight = 100
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 100
	releaseutil.SetReleaseCondition(&contender.release.Status, shipperV1.ReleaseCondition{Type: shipperV1.ReleaseConditionTypeInstalled, Status: coreV1.ConditionTrue, Reason: "", Message: ""})

	incumbent.trafficTarget.Spec.Clusters[0].Weight = 0
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 0
	incumbent.capacityTarget.Spec.Clusters[0].Percent = 0
	incumbent.capacityTarget.Status.Clusters[0].AchievedPercent = 0

	f.addObjects(contender, incumbent)

	f.expectReleaseReleased(contender.release.DeepCopy(), 2)

	f.run()
}

func workingOnContenderCapacity(percent int, wg *sync.WaitGroup, t *testing.T) {
	defer wg.Done()

	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1

	// Working on contender capacity.
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = int32(percent)

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectCapacityNotReady(r, 1, 0, Contender, "minikube")
	f.run()
}

func workingOnContenderTraffic(percent int, wg *sync.WaitGroup, t *testing.T) {
	defer wg.Done()

	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1

	// Desired contender capacity achieved.
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	// Working on contender traffic.
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = uint32(percent)

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectTrafficNotReady(r, 1, 0, Contender, "minikube")
	f.run()

}

func workingOnIncumbentTraffic(percent int, wg *sync.WaitGroup, t *testing.T) {
	defer wg.Done()

	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1

	// Desired contender capacity achieved.
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	// Desired contender traffic achieved.
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	// Working on incumbent traffic.
	incumbent.trafficTarget.Spec.Clusters[0].Weight = 50
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 100 - uint32(percent)

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectTrafficNotReady(r, 1, 0, Incumbent, "minikube")
	f.run()
}

func workingOnIncumbentCapacity(percent int, wg *sync.WaitGroup, t *testing.T) {
	defer wg.Done()

	f := newFixture(t)

	totalReplicaCount := uint(10)
	contender := buildContender(totalReplicaCount)
	incumbent := buildIncumbent(totalReplicaCount)

	contender.release.Spec.TargetStep = 1

	// Desired contender capacity achieved.
	contender.capacityTarget.Spec.Clusters[0].Percent = 50
	contender.capacityTarget.Status.Clusters[0].AchievedPercent = 50

	// Desired contender traffic achieved.
	contender.trafficTarget.Spec.Clusters[0].Weight = 50
	contender.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	// Desired incumbent traffic achieved.
	incumbent.trafficTarget.Spec.Clusters[0].Weight = 50
	incumbent.trafficTarget.Status.Clusters[0].AchievedTraffic = 50

	// Working on incumbent capacity.
	incumbent.capacityTarget.Spec.Clusters[0].Percent = 50
	incumbent.capacityTarget.Status.Clusters[0].AchievedPercent = 100 - int32(percent)

	f.addObjects(contender, incumbent)

	r := contender.release.DeepCopy()
	f.expectCapacityNotReady(r, 1, 0, Incumbent, "minikube")
	f.run()
}

func TestShouldNotProducePatches(t *testing.T) {
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go workingOnContenderCapacity(i, &wg, t)

		wg.Add(1)
		go workingOnContenderTraffic(i, &wg, t)

		wg.Add(1)
		go workingOnIncumbentTraffic(i, &wg, t)

		wg.Add(1)
		go workingOnIncumbentCapacity(i, &wg, t)
	}
	wg.Wait()
}

type fixture struct {
	t                     *testing.T
	client                *shipperfake.Clientset
	discovery             *fakediscovery.FakeDiscovery
	clientPool            *dynamicfake.FakeClientPool
	actions               []kubetesting.Action
	objects               []runtime.Object
	receivedEvents        []string
	expectedOrderedEvents []string
	recorder              *record.FakeRecorder
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, receivedEvents: []string{}}
}

func (f *fixture) addObjects(releaseInfos ...*releaseInfo) {
	for _, r := range releaseInfos {
		f.objects = append(f.objects,
			r.release.DeepCopy(),
			r.capacityTarget.DeepCopy(),
			r.installationTarget.DeepCopy(),
			r.trafficTarget.DeepCopy())
	}
}

func (f *fixture) newController() (*Controller, shipperinformers.SharedInformerFactory) {

	f.objects = append(f.objects, app)
	f.client = shipperfake.NewSimpleClientset(f.objects...)

	fakeDiscovery, _ := f.client.Discovery().(*fakediscovery.FakeDiscovery)
	f.discovery = fakeDiscovery
	f.discovery.Resources = []*metaV1.APIResourceList{
		{
			GroupVersion: "shipper.booking.com/v1",
			APIResources: []metaV1.APIResource{
				{
					Kind:       "Application",
					Namespaced: true,
					Name:       "applications",
				},
				{
					Kind:       "Release",
					Namespaced: true,
					Name:       "releases",
				},
				{
					Kind:       "CapacityTarget",
					Namespaced: true,
					Name:       "capacitytargets",
				},
				{
					Kind:       "InstallationTarget",
					Namespaced: true,
					Name:       "installationtargets",
				},
				{
					Kind:       "TrafficTarget",
					Namespaced: true,
					Name:       "traffictargets",
				},
			},
		},
	}

	const syncPeriod time.Duration = 0
	shipperInformerFactory := shipperinformers.NewSharedInformerFactory(f.client, syncPeriod)

	f.clientPool = &dynamicfake.FakeClientPool{
		Fake: f.client.Fake,
	}

	f.recorder = record.NewFakeRecorder(42)

	c := NewController(f.client, shipperInformerFactory, f.clientPool, f.recorder)

	return c, shipperInformerFactory
}

func (f *fixture) run() {
	c, i := f.newController()

	stopCh := make(chan struct{})
	defer close(stopCh)

	i.Start(stopCh)
	i.WaitForCacheSync(stopCh)

	wait.PollUntil(
		10*time.Millisecond,
		func() (bool, error) { return c.releaseWorkqueue.Len() >= 1, nil },
		stopCh,
	)

	readyCh := make(chan struct{})
	go func() {
		for e := range f.recorder.Events {
			f.receivedEvents = append(f.receivedEvents, e)
		}
		close(readyCh)
	}()

	c.processNextReleaseWorkItem()
	c.processNextAppWorkItem()
	close(f.recorder.Events)
	<-readyCh

	actual := shippertesting.FilterActions(f.clientPool.Actions())
	shippertesting.CheckActions(f.actions, actual, f.t)
	shippertesting.CheckEvents(f.expectedOrderedEvents, f.receivedEvents, f.t)
}

func (f *fixture) expectCapacityStatusPatch(ct *shipperV1.CapacityTarget, r *shipperV1.Release, value uint, totalReplicaCount uint, role role) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("capacitytargets")
	newSpec := map[string]interface{}{
		"spec": shipperV1.CapacityTargetSpec{
			Clusters: []shipperV1.ClusterCapacityTarget{
				{Name: "minikube", Percent: int32(value), TotalReplicaCount: int32(totalReplicaCount)},
			},
		},
	}
	patch, _ := json.Marshal(newSpec)
	action := kubetesting.NewPatchAction(gvr, ct.GetNamespace(), ct.GetName(), patch)
	f.actions = append(f.actions, action)

	step := r.Spec.TargetStep

	var strategyConditions conditions.StrategyConditionsMap

	if role == Contender {
		strategyConditions = conditions.NewStrategyConditions(
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:    shipperV1.StrategyConditionContenderAchievedCapacity,
				Status:  coreV1.ConditionFalse,
				Step:    step,
				Reason:  conditions.ClustersNotReady,
				Message: "clusters pending capacity adjustments: [minikube]",
			},
		)
	} else {
		strategyConditions = conditions.NewStrategyConditions(
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedTraffic,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionIncumbentAchievedTraffic,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:    shipperV1.StrategyConditionIncumbentAchievedCapacity,
				Status:  coreV1.ConditionFalse,
				Step:    step,
				Reason:  conditions.ClustersNotReady,
				Message: "clusters pending capacity adjustments: [minikube]",
			},
		)
	}

	r.Status.Strategy = &shipperV1.ReleaseStrategyStatus{
		Conditions: strategyConditions.AsReleaseStrategyConditions(),
		State:      strategyConditions.AsReleaseStrategyState(r.Spec.TargetStep, true, false),
	}
	newStatus := map[string]interface{}{
		"status": r.Status,
	}
	patch, _ = json.Marshal(newStatus)
	action = kubetesting.NewPatchAction(
		shipperV1.SchemeGroupVersion.WithResource("releases"),
		r.GetNamespace(),
		r.GetName(),
		patch)
	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{}
}

func (f *fixture) expectTrafficStatusPatch(tt *shipperV1.TrafficTarget, r *shipperV1.Release, value uint32, role role) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("traffictargets")
	newSpec := map[string]interface{}{
		"spec": shipperV1.TrafficTargetSpec{
			Clusters: []shipperV1.ClusterTrafficTarget{
				{Name: "minikube", Weight: value},
			},
		},
	}
	patch, _ := json.Marshal(newSpec)
	action := kubetesting.NewPatchAction(gvr, tt.GetNamespace(), tt.GetName(), patch)
	f.actions = append(f.actions, action)

	step := r.Spec.TargetStep

	var strategyConditions conditions.StrategyConditionsMap

	if role == Contender {
		strategyConditions = conditions.NewStrategyConditions(
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:    shipperV1.StrategyConditionContenderAchievedTraffic,
				Status:  coreV1.ConditionFalse,
				Step:    step,
				Reason:  conditions.ClustersNotReady,
				Message: "clusters pending traffic adjustments: [minikube]",
			},
		)
	} else {
		strategyConditions = conditions.NewStrategyConditions(
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:   shipperV1.StrategyConditionContenderAchievedTraffic,
				Status: coreV1.ConditionTrue,
				Step:   step,
			},
			shipperV1.ReleaseStrategyCondition{
				Type:    shipperV1.StrategyConditionIncumbentAchievedTraffic,
				Status:  coreV1.ConditionFalse,
				Step:    step,
				Reason:  conditions.ClustersNotReady,
				Message: "clusters pending traffic adjustments: [minikube]",
			},
		)
	}

	r.Status.Strategy = &shipperV1.ReleaseStrategyStatus{
		Conditions: strategyConditions.AsReleaseStrategyConditions(),
		State:      strategyConditions.AsReleaseStrategyState(r.Spec.TargetStep, true, false),
	}
	newStatus := map[string]interface{}{
		"status": r.Status,
	}
	patch, _ = json.Marshal(newStatus)
	action = kubetesting.NewPatchAction(
		shipperV1.SchemeGroupVersion.WithResource("releases"),
		r.GetNamespace(),
		r.GetName(),
		patch)
	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{}
}

func (f *fixture) expectReleaseReleased(rel *shipperV1.Release, targetStep int32) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("releases")
	newStatus := map[string]interface{}{
		"status": shipperV1.ReleaseStatus{
			AchievedStep: &shipperV1.AchievedStep{
				Step: targetStep,
				Name: rel.Environment.Strategy.Steps[targetStep].Name,
			},
			Conditions: []shipperV1.ReleaseCondition{
				{Type: shipperV1.ReleaseConditionTypeComplete, Status: coreV1.ConditionTrue},
				{Type: shipperV1.ReleaseConditionTypeInstalled, Status: coreV1.ConditionTrue},
				{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
			},
			Strategy: &shipperV1.ReleaseStrategyStatus{
				State: shipperV1.ReleaseStrategyState{
					WaitingForInstallation: shipperV1.StrategyStateFalse,
					WaitingForCommand:      shipperV1.StrategyStateFalse,
					WaitingForTraffic:      shipperV1.StrategyStateFalse,
					WaitingForCapacity:     shipperV1.StrategyStateFalse,
				},
				// The following conditions are sorted alphabetically by Type
				Conditions: []shipperV1.ReleaseStrategyCondition{
					{
						Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
						Status: coreV1.ConditionTrue,
						Step:   targetStep,
					},
					{
						Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
						Status: coreV1.ConditionTrue,
						Step:   targetStep,
					},
					{
						Type:   shipperV1.StrategyConditionContenderAchievedTraffic,
						Status: coreV1.ConditionTrue,
						Step:   targetStep,
					},
					{
						Type:   shipperV1.StrategyConditionIncumbentAchievedCapacity,
						Status: coreV1.ConditionTrue,
						Step:   targetStep,
					},
					{
						Type:   shipperV1.StrategyConditionIncumbentAchievedTraffic,
						Status: coreV1.ConditionTrue,
						Step:   targetStep,
					},
				},
			},
		},
	}

	patch, _ := json.Marshal(newStatus)
	action := kubetesting.NewPatchAction(gvr, rel.GetNamespace(), rel.GetName(), patch)

	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{
		fmt.Sprintf("Normal StrategyApplied step [%d] finished", targetStep),
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForCapacity" transitioned to "False"`,
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForCommand" transitioned to "False"`,
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForInstallation" transitioned to "False"`,
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForTraffic" transitioned to "False"`,
	}
}

func (f *fixture) expectReleaseWaitingForCommand(rel *shipperV1.Release, step int32) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("releases")
	newStatus := map[string]interface{}{
		"status": shipperV1.ReleaseStatus{
			AchievedStep: &shipperV1.AchievedStep{
				Step: step,
				Name: rel.Environment.Strategy.Steps[step].Name,
			},
			Conditions: []shipperV1.ReleaseCondition{
				{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
			},
			Strategy: &shipperV1.ReleaseStrategyStatus{
				State: shipperV1.ReleaseStrategyState{
					WaitingForInstallation: shipperV1.StrategyStateFalse,
					WaitingForCommand:      shipperV1.StrategyStateTrue,
					WaitingForTraffic:      shipperV1.StrategyStateFalse,
					WaitingForCapacity:     shipperV1.StrategyStateFalse,
				},
				Conditions: []shipperV1.ReleaseStrategyCondition{
					{
						Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
						Status: coreV1.ConditionTrue,
						Step:   step,
					},
					{
						Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
						Status: coreV1.ConditionTrue,
						Step:   step,
					},
					{
						Type:   shipperV1.StrategyConditionContenderAchievedTraffic,
						Status: coreV1.ConditionTrue,
						Step:   step,
					},
					{
						Type:   shipperV1.StrategyConditionIncumbentAchievedCapacity,
						Status: coreV1.ConditionTrue,
						Step:   step,
					},
					{
						Type:   shipperV1.StrategyConditionIncumbentAchievedTraffic,
						Status: coreV1.ConditionTrue,
						Step:   step,
					},
				},
			},
		},
	}

	patch, _ := json.Marshal(newStatus)
	action := kubetesting.NewPatchAction(gvr, rel.GetNamespace(), rel.GetName(), patch)
	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{
		fmt.Sprintf("Normal StrategyApplied step [%d] finished", step),
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForCapacity" transitioned to "False"`,
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForCommand" transitioned to "True"`,
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForInstallation" transitioned to "False"`,
		`Normal ReleaseStateTransitioned Release "test-namespace/0.0.2" had its state "WaitingForTraffic" transitioned to "False"`,
	}
}

// NOTE(btyler): when we add tests to use this function with a wider set of use
// cases, we'll need a "pint32(int32) *int32" func to let us take pointers to
// literals
func (f *fixture) expectInstallationNotReady(rel *shipperV1.Release, achievedStepIndex *int32, targetStepIndex int32, role role) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("releases")

	var achievedStep *shipperV1.AchievedStep
	if achievedStepIndex != nil {
		achievedStep = &shipperV1.AchievedStep{
			Step: *achievedStepIndex,
			Name: rel.Environment.Strategy.Steps[*achievedStepIndex].Name,
		}
	}

	newStatus := map[string]interface{}{
		"status": shipperV1.ReleaseStatus{
			AchievedStep: achievedStep,
			Conditions: []shipperV1.ReleaseCondition{
				{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
			},
			Strategy: &shipperV1.ReleaseStrategyStatus{
				State: shipperV1.ReleaseStrategyState{
					WaitingForInstallation: shipperV1.StrategyStateTrue,
					WaitingForCommand:      shipperV1.StrategyStateFalse,
					WaitingForTraffic:      shipperV1.StrategyStateFalse,
					WaitingForCapacity:     shipperV1.StrategyStateFalse,
				},
				Conditions: []shipperV1.ReleaseStrategyCondition{
					{
						Type:    shipperV1.StrategyConditionContenderAchievedInstallation,
						Status:  coreV1.ConditionFalse,
						Reason:  conditions.ClustersNotReady,
						Step:    targetStepIndex,
						Message: "clusters pending installation: [broken-installation-cluster]",
					},
				},
			},
		},
	}

	patch, _ := json.Marshal(newStatus)
	action := kubetesting.NewPatchAction(gvr, rel.GetNamespace(), rel.GetName(), patch)

	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{}
}

func (f *fixture) expectCapacityNotReady(rel *shipperV1.Release, targetStep, achievedStepIndex int32, role role, brokenClusterName string) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("releases")

	var newStatus map[string]interface{}

	var achievedStep *shipperV1.AchievedStep
	if achievedStepIndex != 0 {
		achievedStep = &shipperV1.AchievedStep{
			Step: achievedStepIndex,
			Name: rel.Environment.Strategy.Steps[achievedStepIndex].Name,
		}
	}

	if role == Contender {
		newStatus = map[string]interface{}{
			"status": shipperV1.ReleaseStatus{
				AchievedStep: achievedStep,
				Conditions: []shipperV1.ReleaseCondition{
					{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
				},
				Strategy: &shipperV1.ReleaseStrategyStatus{
					State: shipperV1.ReleaseStrategyState{
						WaitingForInstallation: shipperV1.StrategyStateFalse,
						WaitingForCommand:      shipperV1.StrategyStateFalse,
						WaitingForTraffic:      shipperV1.StrategyStateFalse,
						WaitingForCapacity:     shipperV1.StrategyStateTrue,
					},
					Conditions: []shipperV1.ReleaseStrategyCondition{
						{
							Type:    shipperV1.StrategyConditionContenderAchievedCapacity,
							Status:  coreV1.ConditionFalse,
							Reason:  conditions.ClustersNotReady,
							Message: fmt.Sprintf("clusters pending capacity adjustments: [%s]", brokenClusterName),
							Step:    targetStep,
						},
						{
							Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
					},
				},
			},
		}
	} else {
		newStatus = map[string]interface{}{
			"status": shipperV1.ReleaseStatus{
				AchievedStep: achievedStep,
				Conditions: []shipperV1.ReleaseCondition{
					{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
				},
				Strategy: &shipperV1.ReleaseStrategyStatus{
					State: shipperV1.ReleaseStrategyState{
						WaitingForInstallation: shipperV1.StrategyStateFalse,
						WaitingForCommand:      shipperV1.StrategyStateFalse,
						WaitingForTraffic:      shipperV1.StrategyStateFalse,
						WaitingForCapacity:     shipperV1.StrategyStateTrue,
					},
					Conditions: []shipperV1.ReleaseStrategyCondition{
						{
							Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:   shipperV1.StrategyConditionContenderAchievedTraffic,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:    shipperV1.StrategyConditionIncumbentAchievedCapacity,
							Status:  coreV1.ConditionFalse,
							Reason:  conditions.ClustersNotReady,
							Step:    targetStep,
							Message: fmt.Sprintf("clusters pending capacity adjustments: [%s]", brokenClusterName),
						},
						{
							Type:   shipperV1.StrategyConditionIncumbentAchievedTraffic,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
					},
				},
			},
		}
	}

	patch, _ := json.Marshal(newStatus)
	action := kubetesting.NewPatchAction(gvr, rel.GetNamespace(), rel.GetName(), patch)

	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{}
}

func (f *fixture) expectTrafficNotReady(rel *shipperV1.Release, targetStep, achievedStepIndex int32, role role, brokenClusterName string) {
	gvr := shipperV1.SchemeGroupVersion.WithResource("releases")
	var newStatus map[string]interface{}

	var achievedStep *shipperV1.AchievedStep
	if achievedStepIndex != 0 {
		achievedStep = &shipperV1.AchievedStep{
			Step: achievedStepIndex,
			Name: rel.Environment.Strategy.Steps[achievedStepIndex].Name,
		}
	}

	if role == Contender {
		newStatus = map[string]interface{}{
			"status": shipperV1.ReleaseStatus{
				AchievedStep: achievedStep,
				Conditions: []shipperV1.ReleaseCondition{
					{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
				},
				Strategy: &shipperV1.ReleaseStrategyStatus{
					State: shipperV1.ReleaseStrategyState{
						WaitingForInstallation: shipperV1.StrategyStateFalse,
						WaitingForCommand:      shipperV1.StrategyStateFalse,
						WaitingForTraffic:      shipperV1.StrategyStateTrue,
						WaitingForCapacity:     shipperV1.StrategyStateFalse,
					},
					Conditions: []shipperV1.ReleaseStrategyCondition{
						{
							Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:    shipperV1.StrategyConditionContenderAchievedTraffic,
							Status:  coreV1.ConditionFalse,
							Reason:  conditions.ClustersNotReady,
							Message: fmt.Sprintf("clusters pending traffic adjustments: [%s]", brokenClusterName),
							Step:    targetStep,
						},
					},
				},
			},
		}
	} else {
		newStatus = map[string]interface{}{
			"status": shipperV1.ReleaseStatus{
				AchievedStep: achievedStep,
				Conditions: []shipperV1.ReleaseCondition{
					{Type: shipperV1.ReleaseConditionTypeScheduled, Status: coreV1.ConditionTrue},
				},
				Strategy: &shipperV1.ReleaseStrategyStatus{
					State: shipperV1.ReleaseStrategyState{
						WaitingForInstallation: shipperV1.StrategyStateFalse,
						WaitingForCommand:      shipperV1.StrategyStateFalse,
						WaitingForTraffic:      shipperV1.StrategyStateTrue,
						WaitingForCapacity:     shipperV1.StrategyStateFalse,
					},
					Conditions: []shipperV1.ReleaseStrategyCondition{
						{
							Type:   shipperV1.StrategyConditionContenderAchievedCapacity,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:   shipperV1.StrategyConditionContenderAchievedInstallation,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:   shipperV1.StrategyConditionContenderAchievedTraffic,
							Status: coreV1.ConditionTrue,
							Step:   targetStep,
						},
						{
							Type:    shipperV1.StrategyConditionIncumbentAchievedTraffic,
							Status:  coreV1.ConditionFalse,
							Reason:  conditions.ClustersNotReady,
							Message: fmt.Sprintf("clusters pending traffic adjustments: [%s]", brokenClusterName),
							Step:    targetStep,
						},
					},
				},
			},
		}
	}

	patch, _ := json.Marshal(newStatus)
	action := kubetesting.NewPatchAction(gvr, rel.GetNamespace(), rel.GetName(), patch)

	f.actions = append(f.actions, action)

	f.expectedOrderedEvents = []string{}
}
