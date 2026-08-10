package models

import "fmt"

// How the OAuth client Secret a service authenticates with comes into being.
// This says nothing about where the OIDC provider is: those coordinates live in
// platform.oidc and are the same whichever value this takes.
const (
	// ClientProvisioningExisting: somebody else made the Secret and the package
	// is handed its name. local-secrets-provider on the sandbox, External
	// Secrets projecting from a vault elsewhere.
	ClientProvisioningExisting = "existing"
	// ClientProvisioningKubauth: the package generates the Secret and posts its
	// own OidcClient CR for kubauth to pick up.
	ClientProvisioningKubauth = "kubauth"
)

// PlatformIdentity is the platform.identity block of the platform Context.
//
// It is written rather than inferred from what the cluster happens to carry: a
// platform that silently changes behaviour because a CRD appeared is one nobody
// can reason about, and "kubauth is installed" is not the same statement as
// "kubauth is what provisions our clients".
type PlatformIdentity struct {
	ClientProvisioning string `json:"clientProvisioning"`
	// KubauthNamespace is where the OidcClient CRs go. Required by, and only
	// meaningful for, the kubauth mode.
	KubauthNamespace string `json:"kubauthNamespace,omitempty"`
}

// Validate rejects a block that cannot be acted on. A misconfiguration caught
// here is a clear message at startup; the same one caught later is a service
// that deploys and then cannot authenticate anyone.
func (i *PlatformIdentity) Validate() error {
	switch i.ClientProvisioning {
	case ClientProvisioningExisting:
		return nil
	case ClientProvisioningKubauth:
		if i.KubauthNamespace == "" {
			return fmt.Errorf("platform.identity.kubauthNamespace is required when clientProvisioning is %q", ClientProvisioningKubauth)
		}
		return nil
	default:
		return fmt.Errorf("platform.identity.clientProvisioning must be %q or %q, got %q",
			ClientProvisioningExisting, ClientProvisioningKubauth, i.ClientProvisioning)
	}
}

// ProvisionsWithKubauth reports whether this platform posts OidcClient CRs.
func (i *PlatformIdentity) ProvisionsWithKubauth() bool {
	return i != nil && i.ClientProvisioning == ClientProvisioningKubauth
}
