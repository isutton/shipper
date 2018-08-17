package v1

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	shipperv1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	shippercontroller "github.com/bookingcom/shipper/pkg/controller"
	apputil "github.com/bookingcom/shipper/pkg/util/application"
	releaseutil "github.com/bookingcom/shipper/pkg/util/release"
)

// ReleaseListerExpansion allows custom methods to be added to
// ReleaseLister.
type ReleaseListerExpansion interface{}

// ReleaseNamespaceListerExpansion allows custom methods to be added to
// ReleaseNamespaceLister.
type ReleaseNamespaceListerExpansion interface {
	// ReleasesForApplication returns Releases related to the given application
	// name ordered by generation.
	ReleasesForApplication(appName string) ([]*shipperv1.Release, error)

	// ContenderForApplication returns the contender Release for the given
	// application name.
	ContenderForApplication(appName string) (*shipperv1.Release, error)

	// IncumbentForApplication returns the incumbent Release for the given
	// application name.
	IncumbentForApplication(appName string) (*shipperv1.Release, error)

	// ReleaseForInstallationTarget returns the Release associated with given
	// InstallationTarget. The relationship is established through owner
	// references.
	ReleaseForInstallationTarget(it *shipperv1.InstallationTarget) (*shipperv1.Release, error)
}

func (s releaseNamespaceLister) ReleasesForApplication(appName string) ([]*shipperv1.Release, error) {
	selector := labels.Set{shipperv1.AppLabel: appName}.AsSelector()
	selectedRels, err := s.List(selector)
	if err != nil {
		return nil, err
	}
	for _, e := range selectedRels {
		_, err := releaseutil.GetGeneration(e)
		if err != nil {
			return nil, err
		}
	}
	return selectedRels, nil
}

func (s releaseNamespaceLister) ContenderForApplication(appName string) (*shipperv1.Release, error) {
	rels, err := s.ReleasesForApplication(appName)
	if err != nil {
		return nil, err
	}
	sort.Sort(releaseutil.ByGenerationDescending(rels))
	return apputil.GetContender(appName, rels)
}

func (s releaseNamespaceLister) IncumbentForApplication(appName string) (*shipperv1.Release, error) {
	rels, err := s.ReleasesForApplication(appName)
	if err != nil {
		return nil, err
	}
	sort.Sort(releaseutil.ByGenerationDescending(rels))
	return apputil.GetIncumbent(appName, rels)
}

func (s releaseNamespaceLister) ReleaseForInstallationTarget(it *shipperv1.InstallationTarget) (*shipperv1.Release, error) {
	owner, err := extractOwnerReference(it.ObjectMeta)
	if err != nil {
		return nil, err
	}

	rel, err := s.Get(owner.Name)
	if err != nil {
		return nil, err
	}

	if rel.UID != owner.UID {
		return nil, shippercontroller.NewWrongOwnerReferenceError(it.Name, it.UID, rel.UID)
	}

	return rel, nil
}

// extractOwnerReference returns an owner reference for the given object meta,
// or an error in the case there are multiple or no owner references.
func extractOwnerReference(it metav1.ObjectMeta) (*metav1.OwnerReference, error) {
	if n := len(it.OwnerReferences); n != 1 {
		return nil, shippercontroller.NewMultipleOwnerReferencesError(it.Name, n)
	}
	return &it.OwnerReferences[0], nil
}
