package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// revokedReason says why session must end because its client was revoked
// (its Pairing deleted), or "" if it must not. moonlight-proxy refuses a
// revoked client's requests, but only ending the Session stops a stream it
// already has.
func (c *SessionController) revokedReason(ctx context.Context, session *v1alpha1types.Session) (string, error) {
	name := session.Spec.PairingReference.Name
	if name == "" {
		// Not started by a Moonlight client (made by hand); nothing to revoke.
		return "", nil
	}
	_, err := c.PairingInformer.Namespaced(session.Namespace).Get(name)
	if err == nil {
		return "", nil
	} else if !errors.IsNotFound(err) {
		return "", fmt.Errorf("failed to get pairing %s/%s: %w", session.Namespace, name, err)
	}
	// The Pairing and Session watches are separate streams, so a Session
	// can reach our cache before its Pairing does: ask the API server.
	_, err = c.PairingClient.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return "", nil
	case errors.IsNotFound(err):
		return fmt.Sprintf("its client's pairing %s was deleted", name), nil
	default:
		return "", fmt.Errorf("failed to get pairing %s/%s: %w", session.Namespace, name, err)
	}
}

// reconcilePairing re-reconciles the Sessions of a deleted Pairing, so
// revoking a client ends its stream at once.
func (c *SessionController) reconcilePairing(namespace, name string, pairing *v1alpha1types.Pairing) error {
	if pairing != nil {
		return nil
	}
	sessions, err := c.SessionInformer.Namespaced(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}
	for _, s := range sessions {
		if s.Spec.PairingReference.Name == name {
			c.controller.Enqueue(namespace, s.Name)
		}
	}
	return nil
}
