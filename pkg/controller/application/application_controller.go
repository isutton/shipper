package application

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	shipperv1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	clientset "github.com/bookingcom/shipper/pkg/client/clientset/versioned"
	informers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	listers "github.com/bookingcom/shipper/pkg/client/listers/shipper/v1"
	"github.com/bookingcom/shipper/pkg/conditions"
	releaseutil "github.com/bookingcom/shipper/pkg/util/release"
	apputil "github.com/bookingcom/shipper/pkg/util/application"
	"github.com/bookingcom/shipper/pkg/errors"
)

const (
	AgentName                   = "application-controller"
	DefaultRevisionHistoryLimit = 20
	MinRevisionHistoryLimit     = 1
	MaxRevisionHistoryLimit     = 1000

	// maxRetries is the number of times an Application will be retried before we
	// drop it out of the app workqueue. The number is chosen with the default rate
	// limiter in mind. This results in the following backoff times: 5ms, 10ms,
	// 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s.
	maxRetries = 11
)

// Controller is a Kubernetes controller that creates Releases from
// Applications.
type Controller struct {
	shipperClientset clientset.Interface

	appLister    listers.ApplicationLister
	appSynced    cache.InformerSynced
	appWorkqueue workqueue.RateLimitingInterface

	relLister listers.ReleaseLister
	relSynced cache.InformerSynced

	recorder record.EventRecorder
}

// NewController returns a new Application controller.
func NewController(
	shipperClientset clientset.Interface,
	shipperInformerFactory informers.SharedInformerFactory,
	recorder record.EventRecorder,
) *Controller {
	appInformer := shipperInformerFactory.Shipper().V1().Applications()
	relInformer := shipperInformerFactory.Shipper().V1().Releases()

	c := &Controller{
		shipperClientset: shipperClientset,

		appLister:    appInformer.Lister(),
		appSynced:    appInformer.Informer().HasSynced,
		appWorkqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "application_controller_applications"),

		relLister: relInformer.Lister(),
		relSynced: relInformer.Informer().HasSynced,

		recorder: recorder,
	}

	appInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueApp,
		UpdateFunc: func(_, new interface{}) {
			c.enqueueApp(new)
		},
		DeleteFunc: c.enqueueApp,
	})

	relInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueRel,
		UpdateFunc: func(old, new interface{}) {
			oldRel, oldOk := old.(*shipperv1.Release)
			newRel, newOk := new.(*shipperv1.Release)
			if oldOk && newOk && oldRel.ResourceVersion == newRel.ResourceVersion {
				glog.V(4).Info("Received Release re-sync Update")
				return
			}

			c.enqueueRel(newRel)
		},
		DeleteFunc: c.enqueueRel,
	})

	return c
}

// Run starts Application controller workers and blocks until stopCh is
// closed.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer c.appWorkqueue.ShutDown()

	glog.V(2).Info("Starting Application controller")
	defer glog.V(2).Info("Shutting down Application controller")

	if !cache.WaitForCacheSync(stopCh, c.appSynced, c.relSynced) {
		runtime.HandleError(fmt.Errorf("failed to sync caches for the Application controller"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.applicationWorker, time.Second, stopCh)
	}

	glog.V(2).Info("Started Application controller")

	<-stopCh
}

func (c *Controller) applicationWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.appWorkqueue.Get()
	if shutdown {
		return false
	}

	defer c.appWorkqueue.Done(obj)

	var (
		key string
		ok  bool
	)

	if key, ok = obj.(string); !ok {
		c.appWorkqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("invalid object key (will not retry): %#v", obj))
		return true
	}

	if shouldRetry := c.syncApplication(key); shouldRetry {
		if c.appWorkqueue.NumRequeues(key) >= maxRetries {
			// Drop the Application's key out of the workqueue and thus reset its
			// backoff. This limits the time a "broken" object can hog a worker.
			glog.Warningf("Application %q has been retried too many times, dropping from the queue", key)
			c.appWorkqueue.Forget(key)

			return true
		}

		c.appWorkqueue.AddRateLimited(key)

		return true
	}

	glog.V(4).Infof("Successfully synced Application %q", key)
	c.appWorkqueue.Forget(obj)

	return true
}

func (c *Controller) enqueueRel(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	rel, ok := obj.(*shipperv1.Release)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a shipperv1.Release: %#v", obj))
		return
	}

	if n := len(rel.OwnerReferences); n != 1 {
		runtime.HandleError(fmt.Errorf("expected exactly one owner for Release %q but got %d", key, n))
		return
	}

	owner := rel.OwnerReferences[0]

	c.appWorkqueue.Add(fmt.Sprintf("%s/%s", rel.Namespace, owner.Name))
}

func (c *Controller) enqueueApp(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	c.appWorkqueue.Add(key)
}

func (c *Controller) syncApplication(key string) bool {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid object key (will not retry): %q", key))
		return false
	}

	app, err := c.appLister.Applications(ns).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			glog.V(3).Infof("Application %q has been deleted", key)
			return false
		}

		runtime.HandleError(fmt.Errorf("error syncing Application %q (will retry): %s", key, err))
		return true
	}

	app = app.DeepCopy()

	// Initialize annotations
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}

	if app.Spec.RevisionHistoryLimit == nil {
		var i int32 = DefaultRevisionHistoryLimit
		app.Spec.RevisionHistoryLimit = &i
	}

	// this would be better as OpenAPI validation, but it does not support
	// 'nullable' so it cannot be an optional field
	if *app.Spec.RevisionHistoryLimit < MinRevisionHistoryLimit {
		var min int32 = MinRevisionHistoryLimit
		app.Spec.RevisionHistoryLimit = &min
	}

	if *app.Spec.RevisionHistoryLimit > MaxRevisionHistoryLimit {
		var max int32 = MaxRevisionHistoryLimit
		app.Spec.RevisionHistoryLimit = &max
	}

	var shouldRetry bool
	var rels []*shipperv1.Release

	if rels, err = c.processApplication(app); err != nil {
		shouldRetry = true
		runtime.HandleError(fmt.Errorf("error syncing Application %q (will retry): %s", key, err))
		c.recorder.Event(app, corev1.EventTypeWarning, "FailedApplication", err.Error())
	}

	if newHistory, err := c.getAppHistory(app, rels); err == nil {
		app.Status.History = newHistory
	} else {
		shouldRetry = true
		runtime.HandleError(fmt.Errorf("error fetching history for Application %q (will retry): %s", key, err))
	}

	// TODO(asurikov): change to UpdateStatus when it's available.
	_, err = c.shipperClientset.ShipperV1().Applications(app.Namespace).Update(app)
	if err != nil {
		shouldRetry = true
		runtime.HandleError(fmt.Errorf("error syncing Application %q (will retry): %s", key, err))
	}

	return shouldRetry
}

func (c *Controller) wrapUpApplicationConditions(app *shipperv1.Application, rels []*shipperv1.Release) ([]*shipperv1.Release, error) {
	var (
		contenderRel *shipperv1.Release
		incumbentRel *shipperv1.Release
		err          error
	)

	if rels, err = apputil.SortReleases(rels); err != nil {
		panic(err)
	}

	abortingCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeAborting, corev1.ConditionFalse, "", "")
	apputil.SetApplicationCondition(&app.Status, *abortingCond)
	validHistoryCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeValidHistory, corev1.ConditionTrue, "", "")
	apputil.SetApplicationCondition(&app.Status, *validHistoryCond)
	releaseSyncedCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeReleaseSynced, corev1.ConditionTrue, "", "")
	apputil.SetApplicationCondition(&app.Status, *releaseSyncedCond)
	rollingOutCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeRollingOut, corev1.ConditionUnknown, "", "")

	if contenderRel, err = apputil.GetContender(app.Name, rels); err != nil {
		// There's no contender release yet, so RollingOut condition is
		// Unknown, with error as message.
		rollingOutCond.Message = err.Error()
		goto End
	}

	if incumbentRel, err = apputil.GetIncumbent(app.Name, rels); err != nil && !errors.IsIncumbentNotFoundError(err) {
		// Errors other than incumbent release not found bail out to not
		// report inconsistent status.
		rollingOutCond.Message = err.Error()
		goto End
	}

	if releaseutil.ReleaseComplete(contenderRel) {
		rollingOutCond.Status = corev1.ConditionFalse
		rollingOutCond.Message = fmt.Sprintf(ReleaseActiveMessageFormat, contenderRel.Name)
	} else if incumbentRel != nil {
		rollingOutCond.Status = corev1.ConditionTrue
		rollingOutCond.Message = fmt.Sprintf(TransitioningMessageFormat, incumbentRel.Name, contenderRel.Name)
	} else {
		rollingOutCond.Status = corev1.ConditionTrue
		rollingOutCond.Message = fmt.Sprintf(InitialReleaseMessageFormat, contenderRel.Name)
	}

End:
	apputil.SetApplicationCondition(&app.Status, *rollingOutCond)

	return rels, nil
}

/*
* get all the releases owned by this application
* if 0, create new one (generation 0), return
* if >1, find latest (highest generation #), compare hash of that one to application template hash
* if same, do nothing
* if different, create new release (highest generation # + 1)
 */
func (c *Controller) processApplication(app *shipperv1.Application) ([]*shipperv1.Release, error) {

	var (
		appReleases     []*shipperv1.Release
		contender       *shipperv1.Release
		err             error
		generation      int
		highestObserved int
	)

	if appReleases, err = c.relLister.Releases(app.Namespace).ReleasesForApplication(app.Name); err != nil {
		return nil, err
	}

	// clean up excessive releases regardless of exit path
	defer func() {
		c.cleanUpReleasesForApplication(app, appReleases)
	}()

	if contender, err = apputil.GetContender(app.Name, appReleases); err != nil {
		if errors.IsContenderNotFoundError(err) {
			// Contender doesn't exist, so we are covering the case where Shipper
			// is creating the first release for this application.
			var generation = 0
			if releaseName, iteration, err := c.releaseNameForApplication(app); err != nil {
				return nil, err
			} else if rel, err := c.createReleaseForApplication(app, releaseName, iteration, generation); err != nil {
				releaseSyncedCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeReleaseSynced, corev1.ConditionFalse, conditions.CreateReleaseFailed, fmt.Sprintf("could not create a new release: %q", err))
				apputil.SetApplicationCondition(&app.Status, *releaseSyncedCond)
				return nil, err
			} else {
				appReleases = append(appReleases, rel)
			}
			// It seems that adding an object to the fakeClient doesn't
			// update listers and informers automatically during tests...
			// How should we do it then?
			apputil.SetHighestObservedGeneration(app, generation)
			return c.wrapUpApplicationConditions(app, appReleases)
		}
		return nil, err
	}

	if generation, err = releaseutil.GetGeneration(contender); err != nil {
		validHistoryCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeValidHistory, corev1.ConditionFalse, conditions.BrokenReleaseGeneration, err.Error())
		apputil.SetApplicationCondition(&app.Status, *validHistoryCond)
		return nil, err
	}

	if highestObserved, err = apputil.GetHighestObservedGeneration(app); err != nil {
		validHistoryCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeValidHistory, corev1.ConditionFalse, conditions.BrokenApplicationObservedGeneration, err.Error())
		apputil.SetApplicationCondition(&app.Status, *validHistoryCond)
		return nil, err
	}

	if generation < highestObserved {
		// the current contender's generation is lower than highest observed
		// generation. This usually means that a newer release has been
		// created and deleted. As side-effect of this, the contender's
		// environment will be copied back to the application.
		apputil.CopyEnvironment(app, contender)
		apputil.SetHighestObservedGeneration(app, generation)
		abortingCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeAborting, corev1.ConditionTrue, "", fmt.Sprintf("abort in progress, returning state to release %q", contender.Name))
		apputil.SetApplicationCondition(&app.Status, *abortingCond)
		rollingOutCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeRollingOut, corev1.ConditionTrue, "", "")
		apputil.SetApplicationCondition(&app.Status, *rollingOutCond)
		return appReleases, nil
	}

	if generation > highestObserved {
		// I think the best we can do is bump up to this new high water mark and
		// then proceed as normal if for some reason generation is higher than
		// highest observed. This should be possible in the case of a new release
		// with the higher generation is created but the application object
		// failed to update with the new highest observed generation.
		apputil.SetHighestObservedGeneration(app, generation)
		highestObserved = generation
	}

	if !identicalEnvironments(app.Spec.Template, contender.Environment) {
		// The application's template has been modified and is different than
		// the contender's environment. This means that a new release should
		// be created with the new template.
		highestObserved = highestObserved + 1
		if releaseName, iteration, err := c.releaseNameForApplication(app); err != nil {
			return nil, err
		} else if rel, err := c.createReleaseForApplication(app, releaseName, iteration, highestObserved); err != nil {
			releaseSyncedCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeReleaseSynced, corev1.ConditionFalse, conditions.CreateReleaseFailed, err.Error())
			apputil.SetApplicationCondition(&app.Status, *releaseSyncedCond)
			rollingOutCond := apputil.NewApplicationCondition(shipperv1.ApplicationConditionTypeRollingOut, corev1.ConditionFalse, conditions.CreateReleaseFailed, err.Error())
			apputil.SetApplicationCondition(&app.Status, *rollingOutCond)
			return nil, err
		} else {
			appReleases = append(appReleases, rel)
		}
	}

	apputil.SetHighestObservedGeneration(app, highestObserved)
	return c.wrapUpApplicationConditions(app, appReleases)
}

func (c *Controller) cleanUpReleasesForApplication(app *shipperv1.Application, releases []*shipperv1.Release) {
	var installedReleases []*shipperv1.Release

	// Delete any releases that are not installed. Don't touch the latest release
	// because a release that isn't installed and is the last release just means
	// that the user is rolling out the application.
	for i := 0; i < len(releases)-1; i++ {
		rel := releases[i]
		if releaseutil.ReleaseComplete(releases[i]) {
			installedReleases = append(installedReleases, releases[i])
			continue
		}

		err := c.shipperClientset.ShipperV1().Releases(app.GetNamespace()).Delete(rel.GetName(), &metav1.DeleteOptions{})
		if err != nil {
			if kerrors.IsNotFound(err) {
				// Skip this release: it's already deleted.
				continue
			}

			// Handle the error, but keep on going.
			runtime.HandleError(err)
		}
	}

	// Delete the first X ordered by generation. Bail out on any error so that we
	// maintain the invariant that we always delete oldest first (rather than
	// failing to delete A and successfully deleting B and C in an 'A B C'
	// history).
	overhead := len(installedReleases) - int(*app.Spec.RevisionHistoryLimit)
	for i := 0; i < overhead; i++ {
		rel := installedReleases[i]
		err := c.shipperClientset.ShipperV1().Releases(app.GetNamespace()).Delete(rel.GetName(), &metav1.DeleteOptions{})
		if err != nil {
			runtime.HandleError(err)
			return
		}
	}
}
