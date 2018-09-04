package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

type MultipleOwnerReferencesError string

func (e MultipleOwnerReferencesError) Error() string {
	return string(e)
}

func IsMultipleOwnerReferencesError(err error) bool {
	_, ok := err.(MultipleOwnerReferencesError)
	return ok
}

func NewMultipleOwnerReferencesError(name string, references int) MultipleOwnerReferencesError {
	return MultipleOwnerReferencesError(fmt.Sprintf(
		"expected exactly one owner for object %q, got %d",
		name, references))
}

type WrongOwnerReferenceError string

func (e WrongOwnerReferenceError) Error() string {
	return string(e)
}

func IsWrongOwnerReferenceError(err error) bool {
	_, ok := err.(WrongOwnerReferenceError)
	return ok
}

func NewWrongOwnerReferenceError(name string, expectedUID, gotUID types.UID) WrongOwnerReferenceError {
	return WrongOwnerReferenceError(fmt.Sprintf(
		"the owner Release for InstallationTarget %q is gone; expected UID %s but got %s",
		name,
		expectedUID,
		gotUID,
	))
}
