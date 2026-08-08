package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository/crd"
	"github.com/okdp/okdp-server-new/internal/service/mocks"
)

const testPlatformNamespace = "okdp-system"

func newServiceUnderTest(t *testing.T, crdAvailable bool) (*DefaultConnectionService, *mocks.ConnectionRepository, *mocks.ServiceRepository) {
	t.Helper()

	connectionRepo := &mocks.ConnectionRepository{}
	releaseRepo := &mocks.ServiceRepository{}
	connectionRepo.On("Available", mock.Anything).Return(crdAvailable).Maybe()

	catalog, err := NewEmbeddedConnectionTypeCatalog()
	require.NoError(t, err)

	return NewDefaultConnectionService(connectionRepo, releaseRepo, catalog, testPlatformNamespace), connectionRepo, releaseRepo
}

func postgresRequest() models.ConnectionRequest {
	return models.ConnectionRequest{
		Name:        "warehouse",
		Type:        "database-server",
		Description: "Corporate warehouse",
		Values: map[string]any{
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "db.example.com",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "require",
		},
	}
}

// --- Degradation while the KuboCD connection CRDs are absent ---

func TestCatalogReportsTheCRDAvailability(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	catalog := svc.Catalog(context.Background())

	assert.False(t, catalog.CRDAvailable)
	assert.NotEmpty(t, catalog.Types, "the form must be describable even without the CRDs")
}

func TestListReturnsEmptyWhenTheCRDsAreAbsent(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, false)

	connections, err := svc.List(context.Background(), "demo")

	// An uninstalled optional CRD is the normal state today, not a failure.
	require.NoError(t, err)
	assert.Empty(t, connections)
	connectionRepo.AssertNotCalled(t, "List", mock.Anything, mock.Anything)
}

func TestWritesAreRefusedWhenTheCRDsAreAbsent(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, false)

	_, createErr := svc.Create(context.Background(), "demo", postgresRequest())
	deleteErr := svc.Delete(context.Background(), "demo", "warehouse")

	assert.ErrorIs(t, createErr, ErrConnectionsUnavailable)
	assert.ErrorIs(t, deleteErr, ErrConnectionsUnavailable)
	// Nothing may be half-written: no orphan credentials Secret either.
	connectionRepo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// --- Credentials never reach the Connection spec ---

func TestCreateStoresCredentialsInASecretAndNotInTheSpec(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	var stored *crd.Connection
	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, "demo", "warehouse-credentials", mock.Anything).Return(nil)
	connectionRepo.On("Create", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
		stored = args.Get(2).(*crd.Connection)
	}).Return(nil)

	response, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	require.NotNil(t, stored)
	assert.NotContains(t, stored.Spec.Values, "password", "the password must never be written to the CRD")
	assert.Equal(t, "db.example.com", stored.Spec.Values["host"])
	// The type name is the contract: a package asking for database-server finds
	// this connection by that name.
	assert.Equal(t, "database-server", stored.Spec.Interface)
	assert.Equal(t, "warehouse-credentials", stored.Spec.Values["secretRef"],
		"a consumer reads the Secret name from spec.values, it cannot see annotations")
	assert.NotContains(t, stored.Spec.Values, "username", "the user is a credential, it belongs to the Secret")
	assert.Equal(t, "demo/warehouse-credentials", stored.Annotations[AnnotationCredentialsSecret])

	// The response tells the console which fields are credentials without
	// disclosing them.
	assert.NotContains(t, response.Values, "password")
	assert.Contains(t, response.SecretFields, "password")

	secretCall := findCall(connectionRepo, "CreateOrUpdateSecret")
	require.NotNil(t, secretCall)
	assert.Equal(t, []byte("s3cret"), secretCall.Arguments.Get(3).(map[string][]byte)["password"])
}

func TestCreateRemovesTheSecretWhenTheConnectionIsRejected(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, "demo", "warehouse-credentials", mock.Anything).Return(nil)
	connectionRepo.On("Create", mock.Anything, "demo", mock.Anything).Return(assert.AnError)
	connectionRepo.On("DeleteSecret", mock.Anything, "demo", "warehouse-credentials").Return(nil)

	_, err := svc.Create(context.Background(), "demo", postgresRequest())

	require.Error(t, err)
	connectionRepo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "warehouse-credentials")
}

func TestUpdateOnlyWritesTheResubmittedCredentials(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	existing := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Interface: "database-server"},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(existing, nil)
	connectionRepo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)
	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, "demo", "warehouse-credentials", mock.Anything).Return(nil)

	req := postgresRequest()
	delete(req.Values, "password") // the console does not resend an unchanged credential

	_, err := svc.Update(context.Background(), "demo", "warehouse", req)
	require.NoError(t, err)

	// Only what was resubmitted is written. The stored password survives because
	// CreateOrUpdateSecret merges into the existing Secret instead of replacing it.
	call := findCall(connectionRepo, "CreateOrUpdateSecret")
	require.NotNil(t, call, "the resubmitted username must be written")
	written := call.Arguments.Get(3).(map[string][]byte)
	assert.Contains(t, written, "username")
	assert.NotContains(t, written, "password")
}

// TestUpdateWithNoCredentialAtAllLeavesTheSecretAlone covers the case the
// merge cannot: nothing to write means nothing is written.
func TestUpdateWithNoCredentialAtAllLeavesTheSecretAlone(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	existing := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: "demo"},
		Spec: crd.ConnectionSpec{
			Interface: "database-server",
			Values:    map[string]any{"secretRef": "warehouse-credentials"},
		},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(existing, nil)
	connectionRepo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)

	req := postgresRequest()
	delete(req.Values, "password")
	delete(req.Values, "username")

	_, err := svc.Update(context.Background(), "demo", "warehouse", req)

	require.NoError(t, err)
	connectionRepo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestPlatformConnectionsStoreTheirCredentialsInThePlatformNamespace(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, testPlatformNamespace, "warehouse-credentials", mock.Anything).Return(nil)
	connectionRepo.On("Create", mock.Anything, "", mock.Anything).Return(nil)

	// An empty namespace addresses the cluster-wide scope, which has no
	// namespace of its own to hold a Secret.
	response, err := svc.Create(context.Background(), "", postgresRequest())

	require.NoError(t, err)
	assert.Equal(t, models.ConnectionScopePlatform, response.Scope)
	connectionRepo.AssertCalled(t, "CreateOrUpdateSecret", mock.Anything, testPlatformNamespace, "warehouse-credentials", mock.Anything)
}

// --- Validation ---

func TestCreateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*models.ConnectionRequest)
		message string
	}{
		{
			name:    "name that is not a valid Kubernetes object name",
			mutate:  func(r *models.ConnectionRequest) { r.Name = "Ware House" },
			message: "invalid connection name",
		},
		{
			name:    "unknown type",
			mutate:  func(r *models.ConnectionRequest) { r.Type = "oracle" },
			message: `unknown connection type "oracle"`,
		},
		{
			name:    "type that only comes from a deployed service",
			mutate:  func(r *models.ConnectionRequest) { r.Type = "trino" },
			message: "cannot be declared by hand",
		},
		{
			name:    "value rejected by the schema",
			mutate:  func(r *models.ConnectionRequest) { delete(r.Values, "host") },
			message: "Host is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, connectionRepo, _ := newServiceUnderTest(t, true)
			req := postgresRequest()
			tt.mutate(&req)

			_, err := svc.Create(context.Background(), "demo", req)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
			assert.True(t, IsValidationError(err), "must be reported to the caller as a 400, not a 500")
			connectionRepo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestManagedConnectionsAreNotEditable(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	managed := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "trino", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Interface: "trino", OutputName: "main"},
		Status:     crd.ConnectionStatus{Parent: "demo-trino"},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "trino").Return(managed, nil)

	err := svc.Delete(context.Background(), "demo", "trino")

	require.Error(t, err)
	// The message names the service, so the reader knows what to remove if they
	// really want the connection gone.
	assert.Contains(t, err.Error(), "demo-trino")
	assert.Contains(t, err.Error(), "cannot be deleted")
	connectionRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything, mock.Anything)
}

// The controller fills status.parent only once it has reconciled, so a managed
// connection can legitimately have none — the refusal must still read sensibly.
func TestManagedConnectionWithoutAParentIsStillRefused(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	managed := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "trino", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Interface: "trino", OutputName: "main"},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "trino").Return(managed, nil)

	err := svc.Delete(context.Background(), "demo", "trino")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provided by a deployed service")
	assert.NotContains(t, err.Error(), `""`)
	connectionRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything, mock.Anything)
}

func TestListExcludesManagedConnections(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "warehouse"},
			Spec:       crd.ConnectionSpec{Interface: "database-server"},
		},
		{
			// Owned by a release: it belongs to the internal view.
			ObjectMeta: metav1.ObjectMeta{Name: "trino"},
			Spec:       crd.ConnectionSpec{Interface: "trino", OutputName: "main"},
		},
	}, nil)

	connections, err := svc.List(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, "warehouse", connections[0].Name)
}

// --- Internal connections ---

func trinoRelease() crd.Release {
	return crd.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "demo-trino",
			Labels: map[string]string{crd.LabelService: "trino"},
		},
		Status: crd.ReleaseStatus{Phase: "READY"},
	}
}

func jupyterRelease() crd.Release {
	return crd.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "demo-jupyterhub",
			Labels: map[string]string{crd.LabelService: "jupyterhub"},
		},
		Status: crd.ReleaseStatus{Phase: "READY"},
	}
}

func kubeService(name string, ports ...corev1.ServicePort) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.1", Ports: ports},
	}
}

func TestListInternalKeepsOnlyServicesThatProvideAConnection(t *testing.T) {
	svc, connectionRepo, releaseRepo := newServiceUnderTest(t, false)

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{
		trinoRelease(),
		jupyterRelease(), // a notebook exposes no connection to other services
	}, nil)
	connectionRepo.On("ListKubeServices", mock.Anything, "demo").Return([]corev1.Service{
		kubeService("demo-trino", corev1.ServicePort{Name: "http", Port: 8080}),
	}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, "demo-trino", connections[0].Name)
	assert.Equal(t, "trino", connections[0].Type)
	assert.Equal(t, "Trino", connections[0].TypeDisplay)
	assert.Equal(t, "Ready", connections[0].Status)
	assert.False(t, connections[0].Managed)
}

func TestListInternalResolvesTheEndpointFromTheKubernetesService(t *testing.T) {
	svc, connectionRepo, releaseRepo := newServiceUnderTest(t, false)

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{trinoRelease()}, nil)
	connectionRepo.On("ListKubeServices", mock.Anything, "demo").Return([]corev1.Service{
		// The worker Service must not win over the coordinator one.
		kubeService("demo-trino-worker", corev1.ServicePort{Name: "http", Port: 8080}),
		kubeService("demo-trino", corev1.ServicePort{Name: "metrics", Port: 9090}, corev1.ServicePort{Name: "http", Port: 8080}),
	}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, "demo-trino.demo.svc.cluster.local", connections[0].Host)
	assert.Equal(t, int32(8080), connections[0].Port, "the port named by the type wins over the first one")
	assert.Equal(t, "demo-trino.demo.svc.cluster.local:8080", connections[0].Endpoint)
	assert.Equal(t, "demo-trino.demo.svc.cluster.local", connections[0].Values["host"])
}

func TestListInternalReportsNoEndpointRatherThanGuessingOne(t *testing.T) {
	svc, connectionRepo, releaseRepo := newServiceUnderTest(t, false)

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{trinoRelease()}, nil)
	// Nothing in the namespace backs the release yet (still installing).
	connectionRepo.On("ListKubeServices", mock.Anything, "demo").Return([]corev1.Service{}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Empty(t, connections[0].Endpoint, "an address that would not resolve must not be shown")
	assert.Empty(t, connections[0].Host)
}

func TestListInternalIgnoresHeadlessServices(t *testing.T) {
	svc, connectionRepo, releaseRepo := newServiceUnderTest(t, false)

	headless := kubeService("demo-trino", corev1.ServicePort{Name: "http", Port: 8080})
	headless.Spec.ClusterIP = corev1.ClusterIPNone

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{trinoRelease()}, nil)
	connectionRepo.On("ListKubeServices", mock.Anything, "demo").Return([]corev1.Service{headless}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Empty(t, connections[0].Endpoint, "a headless Service addresses individual pods, not the service")
}

func TestListInternalPrefersTheManagedConnectionOverTheDerivedOne(t *testing.T) {
	// Once the CRDs are installed, what the release controller publishes is
	// authoritative and replaces what we inferred for the same release.
	svc, connectionRepo, releaseRepo := newServiceUnderTest(t, true)

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{trinoRelease()}, nil)
	connectionRepo.On("ListKubeServices", mock.Anything, "demo").Return([]corev1.Service{
		kubeService("demo-trino", corev1.ServicePort{Name: "http", Port: 8080}),
	}, nil)
	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-trino-main"},
			Spec: crd.ConnectionSpec{
				Interface:  "trino",
				OutputName: "main",
				Values:     map[string]any{"host": "trino.demo.svc.cluster.local", "port": float64(8080)},
			},
			Status: crd.ConnectionStatus{Parent: "demo-trino", Phase: "READY"},
		},
	}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1, "the derived entry must not be listed alongside the managed one")
	assert.True(t, connections[0].Managed)
	assert.Equal(t, "demo-trino-main", connections[0].Name)
	assert.Equal(t, "trino.demo.svc.cluster.local:8080", connections[0].Endpoint)
}

// --- Connectivity test ---

func TestTestRejectsBadInputBeforeDialing(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	tests := []struct {
		name    string
		request models.ConnectionTestRequest
		reason  string
	}{
		{
			name:    "unknown type",
			request: models.ConnectionTestRequest{Type: "oracle", Values: map[string]any{}},
			reason:  models.TestReasonInvalidConfig,
		},
		{
			name:    "missing required value",
			request: models.ConnectionTestRequest{Type: "database-server", Values: map[string]any{"host": "db"}},
			reason:  models.TestReasonInvalidConfig,
		},
		{
			name: "type that has no tester",
			request: models.ConnectionTestRequest{Type: "trino", Values: map[string]any{
				"host": "trino.demo.svc.cluster.local", "port": float64(8080),
			}},
			reason: models.TestReasonInvalidConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.Test(context.Background(), tt.request)

			assert.False(t, result.Success)
			assert.Equal(t, tt.reason, result.Reason)
			assert.NotEmpty(t, result.Message)
		})
	}
}

func TestTestReportsAnUnreachableHost(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	result := svc.Test(context.Background(), models.ConnectionTestRequest{
		Type: "database-server",
		Values: map[string]any{
			// Reserved by RFC 6761 to never resolve.
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "nothing.invalid",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "disable",
		},
	})

	assert.False(t, result.Success)
	assert.Contains(t, []string{models.TestReasonUnreachable, models.TestReasonTimeout}, result.Reason)
}

// One type, one form, two drivers: the probe reads the engine field. An engine
// the server has no driver for must say so rather than fall back to a default,
// which would report a MySQL server unreachable on the PostgreSQL port. The
// catalog's enum stops this at the door, so the guard is tested directly.
func TestDatabaseProbeRejectsAnUnknownEngine(t *testing.T) {
	err := testDatabaseServer(context.Background(), connectionValues{
		"engine": "oracle",
		"host":   "db.example.com",
		"port":   float64(1521),
	})

	var failed *testFailure
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, models.TestReasonInvalidConfig, failed.reason)
	assert.Contains(t, failed.message, "oracle")
}

// findCall returns the first recorded call to method, or nil.
func findCall(m *mocks.ConnectionRepository, method string) *mock.Call {
	for i := range m.Calls {
		if m.Calls[i].Method == method {
			return &m.Calls[i]
		}
	}
	return nil
}
