package moonlight

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// libraryBusyMessage is what a launch gets while the Library is open.
const libraryBusyMessage = "The game library is open. Close it (or wait for it to stop) before launching a game"

// libraryStoppingMessage is what a launch gets while the Library shuts down
// (e.g. just after its "Close Library" tile, which is then no longer listed).
const libraryStoppingMessage = "The game library is closing. Try again in a minute or two"

// LibraryBusyCheck refuses a User's launches while the Library pod of the
// library they play from exists, terminating or not: the Library and its
// game sessions share one Steam home, which has a single writer. Another
// library's Library is no obstacle. It lists from the API server, not an
// informer, so a Library pod created a moment ago counts.
func LibraryBusyCheck(pods corev1client.PodInterface) BusyCheck {
	return func(ctx context.Context, user *v1alpha1types.User) (string, error) {
		selector, err := libraryPodSelector(user.LibraryOwner())
		if err != nil {
			return "", err
		}
		list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 1})
		if err != nil {
			return "", fmt.Errorf("failed to list Library pods: %w", err)
		}
		if len(list.Items) > 0 {
			if list.Items[0].DeletionTimestamp != nil {
				return libraryStoppingMessage, nil
			}
			return libraryBusyMessage, nil
		}
		return "", nil
	}
}

// CloseLibraryAppID is the Moonlight app ID of the built-in "Close Library"
// tile, listed while the Library is open so a Library left running in some
// browser can be stopped from Moonlight. It sits just under the catalogue's
// IDs (2^30..2^31-1) and far above hand-written Apps' small ones; an App with
// this ID takes precedence and the tile is not offered.
const CloseLibraryAppID = 1<<30 - 1

// libraryCallTimeout bounds the tile's API calls: /applist used to be served
// from the informer alone, and a hung API server must not hold it.
const libraryCallTimeout = 2 * time.Second

const (
	closeLibraryTitle = "Close Library"
	// libraryClosingStatusCode is the XML status of a "Close Library" launch:
	// nothing streams, and Moonlight shows a non-200 status's message.
	libraryClosingStatusCode  = 400
	libraryClosingMessage     = "Closing the game library. Games can be launched once it has stopped, within two minutes"
	libraryNotOpenMessage     = "The game library is not open"
	libraryCloseFailedMessage = "The game library could not be closed. Try again, or wait for it to stop"
)

// libraryPodSelector selects owner's Library pod ("" the shared one's).
func libraryPodSelector(owner string) (string, error) {
	op, values := selection.DoesNotExist, []string(nil)
	if owner != "" {
		op, values = selection.Equals, []string{owner}
	}
	ownerReq, err := labels.NewRequirement(v1alpha1types.LibraryOwnerLabel, op, values)
	if err != nil {
		return "", fmt.Errorf("building Library pod selector: %w", err)
	}
	return labels.SelectorFromSet(labels.Set{
		v1alpha1types.LibraryPodLabel: v1alpha1types.LibraryPodLabelValue,
	}).Add(*ownerReq).String(), nil
}

// Library is the operator's Library pods as Moonlight users see them: owner
// is the library ("" the shared one).
type Library interface {
	// Open reports whether owner's Library is running and not already stopping.
	Open(ctx context.Context, owner string) (bool, error)
	// Close stops owner's Library, downloads or not. It reports false if
	// there was no Library to close.
	Close(ctx context.Context, owner string) (bool, error)
}

// NewPodLibrary is the Library backed by the operator's Library pod. Close
// deletes the pod; its preStop hook shuts Steam down cleanly, and the pod's
// presence keeps launches refused (LibraryBusyCheck) until it is gone.
func NewPodLibrary(pods corev1client.PodInterface) Library {
	return podLibrary{pods: pods}
}

type podLibrary struct {
	pods corev1client.PodInterface
}

// get returns owner's Library pod, or nil if there is none. It reads the API
// server: the tile should vanish the moment the Library stops.
func (l podLibrary) get(ctx context.Context, owner string) (*corev1.Pod, error) {
	pod, err := l.pods.Get(ctx, v1alpha1types.LibraryPodNameFor(owner), metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get Library pod: %w", err)
	}
	if pod.Labels[v1alpha1types.LibraryPodLabel] != v1alpha1types.LibraryPodLabelValue ||
		v1alpha1types.LibraryOwnerOf(pod.Labels) != owner {
		return nil, nil
	}
	return pod, nil
}

func (l podLibrary) Open(ctx context.Context, owner string) (bool, error) {
	pod, err := l.get(ctx, owner)
	return pod != nil && pod.DeletionTimestamp == nil, err
}

func (l podLibrary) Close(ctx context.Context, owner string) (bool, error) {
	pod, err := l.get(ctx, owner)
	if pod == nil || err != nil {
		return false, err
	}
	if pod.DeletionTimestamp != nil {
		return true, nil
	}
	err = l.pods.Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	if k8serrors.IsNotFound(err) || k8serrors.IsConflict(err) {
		// Gone, or replaced by a Library someone just opened: leave that one.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to delete Library pod: %w", err)
	}
	return true, nil
}
