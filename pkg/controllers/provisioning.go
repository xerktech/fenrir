package controllers

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"slices"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// DefaultLibraryGroupsHeader is the header Authentik's proxy outpost lists
// the signed-in user's groups in, separated by DefaultLibraryGroupsSeparator.
const DefaultLibraryGroupsHeader = "X-Authentik-Groups"

// DefaultLibraryGroupsSeparator separates groups in DefaultLibraryGroupsHeader.
const DefaultLibraryGroupsSeparator = "|"

// Provisioned claims are named after their User.
const (
	provisionedHomeClaimPrefix  = "direwolf-home-"
	provisionedGamesClaimPrefix = "direwolf-games-"
)

// errProvisionRefused: a username the provisioner will not create a User for.
var errProvisionRefused = stderrors.New("not provisioning")

// ClaimTemplate is a provisioned PVC's storage.
type ClaimTemplate struct {
	// StorageClassName empty uses the cluster's default class.
	StorageClassName string
	Size             resource.Quantity
}

// ProvisionerOptions configure the provisioner (all from operator flags).
type ProvisionerOptions struct {
	// Group is the group whose members get a User; empty matches no one.
	Group string
	// AllowedUsers get a User whatever their groups, for proxies that send
	// none. Matched exactly against the user header. With Group empty and no
	// AllowedUsers, provisioning is off.
	AllowedUsers []string
	// GroupsHeader lists the signed-in user's groups, set only by the trusted proxy.
	GroupsHeader string
	// GroupsSeparator separates GroupsHeader's groups.
	GroupsSeparator string
	// UserTemplate is every provisioned User's spec, without library.
	UserTemplate v1alpha1types.UserSpec
	Home, Games  ClaimTemplate
	// SharedHomePVC and SharedGamesPVC are the shared library's claims, which
	// no User's library may use.
	SharedHomePVC, SharedGamesPVC string
}

// Provisioner creates a group member's User, with a library of their own
// (User.spec.library) on two new PVCs. It never changes an existing User or
// claim, and never deletes anything: deprovisioning is an admin's call.
//
// The PVCs get no ownerReference to the User: deleting (or re-creating) a
// User must never garbage-collect a games volume, and a recreated User
// re-adopts its claims.
type Provisioner struct {
	ProvisionerOptions
	namespace string
	users     generic.NamespacedLister[*v1alpha1types.User]
	userAPI   v1alpha1client.UserInterface
	claims    typedcorev1.PersistentVolumeClaimInterface
}

func NewProvisioner(
	namespace string,
	users generic.NamespacedLister[*v1alpha1types.User],
	userAPI v1alpha1client.UserInterface,
	claims typedcorev1.PersistentVolumeClaimInterface,
	opts *ProvisionerOptions,
) *Provisioner {
	return &Provisioner{ProvisionerOptions: *opts, namespace: namespace, users: users, userAPI: userAPI, claims: claims}
}

// ParseProvisionUserTemplate reads a UserSpec from JSON or YAML; empty is an
// empty spec. Unknown fields are refused (a typo would silently drop a
// limit), and so is library: the provisioner sets it.
func ParseProvisionUserTemplate(s string) (v1alpha1types.UserSpec, error) {
	var spec v1alpha1types.UserSpec
	if strings.TrimSpace(s) == "" {
		return spec, nil
	}
	// sigs.k8s.io/yaml reads only the first document, so a second (say, one
	// setting library, or after a "..." end marker) would be dropped
	// silently. Find the one non-empty document with a full YAML parser.
	doc, err := singleYAMLDocument(s)
	if err != nil {
		return spec, err
	}
	if err := yaml.UnmarshalStrict(doc, &spec); err != nil {
		return spec, fmt.Errorf("parsing user template: %w", err)
	}
	if spec.Library != nil {
		return spec, stderrors.New("user template must not set library: each provisioned User gets its own")
	}
	return spec, nil
}

// Enabled reports whether anyone at all may be provisioned.
func (o *ProvisionerOptions) Enabled() bool {
	return o.Group != "" || len(o.AllowedUsers) > 0
}

// Eligible reports whether username, whose groups are the GroupsHeader
// values (r.Header.Values), gets a User: a member of p.Group, or on
// p.AllowedUsers.
func (p *Provisioner) Eligible(username string, groups []string) bool {
	return username != "" && (slices.Contains(p.AllowedUsers, username) || p.InGroup(groups))
}

// InGroup reports whether groups (every GroupsHeader value: oauth2-proxy
// sends one header line per group, others one separated list) names p.Group.
func (p *Provisioner) InGroup(groups []string) bool {
	if p.Group == "" {
		return false
	}
	for _, line := range groups {
		for g := range strings.SplitSeq(line, p.GroupsSeparator) {
			if strings.TrimSpace(g) == p.Group {
				return true
			}
		}
	}
	return false
}

// Ensure returns username's User, creating it and its library's claims if
// it has none. Safe to call concurrently: whoever loses a Create race reads
// the winner's object. An existing User is returned unchanged, with or
// without a library; one whose name another username already holds is
// refused.
func (p *Provisioner) Ensure(ctx context.Context, username string) (*v1alpha1types.User, error) {
	name, err := v1alpha1types.UserNameFor(username)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errProvisionRefused, err)
	}
	if user, getErr := p.users.Get(name); getErr == nil {
		return existingUserFor(user, username)
	} else if !errors.IsNotFound(getErr) {
		return nil, fmt.Errorf("getting user %s: %w", name, getErr)
	}
	// The cache may lag a User created moments ago; check live before
	// creating claims it would never use.
	if live, getErr := p.userAPI.Get(ctx, name, metav1.GetOptions{}); getErr == nil {
		return existingUserFor(live, username)
	} else if !errors.IsNotFound(getErr) {
		return nil, fmt.Errorf("getting user %s: %w", name, getErr)
	}

	lib := LibraryRef{Owner: name, HomePVC: provisionedHomeClaimPrefix + name, GamesPVC: provisionedGamesClaimPrefix + name}
	cached, listErr := p.users.List(labels.Everything())
	if listErr != nil {
		return nil, fmt.Errorf("listing users: %w", listErr)
	}
	users := make([]v1alpha1types.User, 0, len(cached))
	for _, u := range cached {
		users = append(users, *u)
	}
	// The Library would refuse the User's claims (EnsurePod); never create one
	// it can't start.
	if conflict := libraryClaimsConflict(lib, p.SharedHomePVC, p.SharedGamesPVC, users); conflict != nil {
		return nil, fmt.Errorf("%w: %w", errProvisionRefused, conflict)
	}

	// Refuse a claim someone else holds before creating either, so a refused
	// username leaves nothing behind.
	for _, claim := range []string{lib.HomePVC, lib.GamesPVC} {
		if live, getErr := p.claims.Get(ctx, claim, metav1.GetOptions{}); getErr == nil {
			if !claimProvisionedFor(live, username) {
				return nil, fmt.Errorf("%w %q: claim %s exists and is not provisioned for it", errProvisionRefused, username, claim)
			}
		} else if !errors.IsNotFound(getErr) {
			return nil, fmt.Errorf("getting claim %s: %w", claim, getErr)
		}
	}

	// Claims first: a User is never left pointing at claims that don't exist,
	// and claims left by a failed attempt are adopted by the next.
	if claimErr := p.ensureClaim(ctx, lib.HomePVC, p.Home, username); claimErr != nil {
		return nil, claimErr
	}
	if claimErr := p.ensureClaim(ctx, lib.GamesPVC, p.Games, username); claimErr != nil {
		return nil, claimErr
	}

	user := &v1alpha1types.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   p.namespace,
			Labels:      map[string]string{v1alpha1types.ProvisionedLabel: v1alpha1types.ProvisionedLabelValue},
			Annotations: map[string]string{v1alpha1types.UsernameAnnotation: username},
		},
		Spec: *p.UserTemplate.DeepCopy(),
	}
	user.Spec.Library = &v1alpha1types.UserLibrary{HomeClaimName: lib.HomePVC, GamesClaimName: lib.GamesPVC}
	created, err := p.userAPI.Create(ctx, user, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		live, getErr := p.userAPI.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, fmt.Errorf("getting user %s: %w", name, getErr)
		}
		return existingUserFor(live, username)
	}
	if err != nil {
		return nil, fmt.Errorf("creating user %s: %w", name, err)
	}
	klog.Infof("Provisioned User %s for %q", name, username)
	return created, nil
}

func existingUserFor(user *v1alpha1types.User, username string) (*v1alpha1types.User, error) {
	if !user.IsFor(username) {
		return nil, fmt.Errorf("%w %q: user %s belongs to another username", errProvisionRefused, username, user.Name)
	}
	return user, nil
}

// ensureClaim creates a provisioned claim, or accepts one already made for
// username. Any other claim of that name is refused, never shared.
func (p *Provisioner) ensureClaim(ctx context.Context, name string, tmpl ClaimTemplate, username string) error {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   p.namespace,
			Labels:      map[string]string{v1alpha1types.ProvisionedLabel: v1alpha1types.ProvisionedLabelValue},
			Annotations: map[string]string{v1alpha1types.UsernameAnnotation: username},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: tmpl.Size},
			},
		},
	}
	if tmpl.StorageClassName != "" {
		claim.Spec.StorageClassName = &tmpl.StorageClassName
	}
	_, err := p.claims.Create(ctx, claim, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		live, getErr := p.claims.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting claim %s: %w", name, getErr)
		}
		if !claimProvisionedFor(live, username) {
			return fmt.Errorf("%w %q: claim %s exists and is not provisioned for it", errProvisionRefused, username, name)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating claim %s: %w", name, err)
	}
	return nil
}

// claimProvisionedFor reports whether claim was provisioned for username.
func claimProvisionedFor(claim *corev1.PersistentVolumeClaim, username string) bool {
	return claim.Labels[v1alpha1types.ProvisionedLabel] == v1alpha1types.ProvisionedLabelValue &&
		claim.Annotations[v1alpha1types.UsernameAnnotation] == username
}

// singleYAMLDocument returns s's only non-empty YAML document, re-encoded;
// empty (comment-only or null) documents are skipped, and a second
// non-empty one is refused.
func singleYAMLDocument(s string) ([]byte, error) {
	dec := yamlv3.NewDecoder(strings.NewReader(s))
	var found *yamlv3.Node
	for {
		var node yamlv3.Node
		err := dec.Decode(&node)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing user template: %w", err)
		}
		// Only a scalar null is empty: yaml.v3 keeps a "!!null" tag on a
		// mapping, whose content must still count.
		if len(node.Content) == 0 || (node.Content[0].Kind == yamlv3.ScalarNode && node.Content[0].Tag == "!!null") {
			continue
		}
		if node.Content[0].Tag == "!!null" {
			// Re-encoded, it would read as null: its fields would be dropped.
			return nil, stderrors.New("user template must not tag a mapping or list !!null")
		}
		if found != nil {
			return nil, stderrors.New("user template must be a single YAML document")
		}
		found = node.Content[0]
	}
	if found == nil {
		return nil, nil
	}
	out, err := yamlv3.Marshal(found)
	if err != nil {
		return nil, fmt.Errorf("re-encoding user template: %w", err)
	}
	return out, nil
}
