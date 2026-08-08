package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository"
	"github.com/okdp/okdp-server-new/internal/repository/crd"
	"github.com/sirupsen/logrus"
)

// AnnotationCredentialsSecret names the Secret holding the credential fields of
// a connection. They are kept out of spec.values so that what the CRD stores
// stays exactly the shape a KuboCD Interface schema will validate, and so that
// reading a Connection never discloses a password.
const AnnotationCredentialsSecret = "okdp.io/credentials-secret"

// credentialsSecretSuffix mirrors the convention already used by secret stores.
const credentialsSecretSuffix = "-credentials"

// valueSecretRef names the value published so a KuboCD package can bind the
// credentials: the NAME of the Secret holding them, never a value. It matches
// the secretRef field declared by the KuboCD Interfaces, which are the contract
// of record.
//
// A credentialsVersion digest used to be published alongside, so that rotating a
// password changed the Connection and reached running pods. It was dropped: the
// key alone changes nothing unless every consuming package propagates it into a
// pod template annotation, and Reloader already restarts workloads when a Secret
// they mount changes.
const valueSecretRef = "secretRef"

// ConnectionService manages the connections of a project and of the platform.
//
// Throughout, an empty namespace addresses the platform (cluster-wide) scope,
// matching ConnectionRepository.
type ConnectionService interface {
	// Catalog returns the known connection types and whether connections can
	// currently be persisted.
	Catalog(ctx context.Context) models.ConnectionCatalogResponse

	List(ctx context.Context, namespace string) ([]models.ConnectionResponse, error)
	Create(ctx context.Context, namespace string, req models.ConnectionRequest) (*models.ConnectionResponse, error)
	Update(ctx context.Context, namespace, name string, req models.ConnectionRequest) (*models.ConnectionResponse, error)
	Delete(ctx context.Context, namespace, name string) error

	// Test probes a live endpoint with the submitted values. It never touches
	// the cluster, so a connection can be validated before it is created.
	Test(ctx context.Context, req models.ConnectionTestRequest) models.ConnectionTestResult

	// ListInternal returns the connections provided by the services already
	// deployed in a project, consumable by the project's other services.
	ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error)

	// ListSelectable returns the connections a deployment form can offer for an
	// input of the given interface: the project's own plus the platform-wide
	// ones, managed included.
	ListSelectable(ctx context.Context, project, iface string) ([]models.SelectableConnection, error)
}

type DefaultConnectionService struct {
	repo              repository.ConnectionRepository
	releaseRepo       repository.ServiceRepository
	catalog           ConnectionTypeCatalog
	platformNamespace string
}

func NewDefaultConnectionService(
	repo repository.ConnectionRepository,
	releaseRepo repository.ServiceRepository,
	catalog ConnectionTypeCatalog,
	platformNamespace string,
) *DefaultConnectionService {
	return &DefaultConnectionService{
		repo:              repo,
		releaseRepo:       releaseRepo,
		catalog:           catalog,
		platformNamespace: platformNamespace,
	}
}

// ErrConnectionsUnavailable is returned by the write paths while the KuboCD
// connection CRDs are not installed. The handler turns it into a 501 so the
// console can explain the situation instead of showing a generic failure.
var ErrConnectionsUnavailable = fmt.Errorf("the KuboCD connection CRDs are not installed on this cluster")

// ValidationError is a problem with what was submitted, as opposed to a failure
// of the platform. It lets the handler answer 400 rather than 500 without
// having to guess from the message.
type ValidationError struct{ message string }

func (e *ValidationError) Error() string { return e.message }

func invalid(format string, args ...any) error {
	return &ValidationError{message: fmt.Sprintf(format, args...)}
}

// IsValidationError reports whether err is a rejected user input.
func IsValidationError(err error) bool {
	var validationErr *ValidationError
	return errors.As(err, &validationErr)
}

func (s *DefaultConnectionService) Catalog(ctx context.Context) models.ConnectionCatalogResponse {
	return models.ConnectionCatalogResponse{
		Types:        s.catalog.List(),
		CRDAvailable: s.repo.Available(ctx),
	}
}

// --- External connections ---

func (s *DefaultConnectionService) List(ctx context.Context, namespace string) ([]models.ConnectionResponse, error) {
	// Not having the CRDs is the normal state today, not an error: the console
	// shows an empty list and explains why, rather than a failed request.
	if !s.repo.Available(ctx) {
		return []models.ConnectionResponse{}, nil
	}

	connections, err := s.repo.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	result := make([]models.ConnectionResponse, 0, len(connections))
	for i := range connections {
		// Managed connections belong to the internal view; they are owned by a
		// release and must not be editable here.
		if connections[i].IsManaged() {
			continue
		}
		result = append(result, s.toResponse(&connections[i], namespace))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *DefaultConnectionService) Create(ctx context.Context, namespace string, req models.ConnectionRequest) (*models.ConnectionResponse, error) {
	connectionType, values, err := s.validateRequest(ctx, req, false)
	if err != nil {
		return nil, err
	}

	public, secrets := splitValues(connectionType, values)
	secretNamespace := s.credentialsNamespace(namespace)
	secretName := req.Name + credentialsSecretSuffix

	if len(secrets) > 0 {
		if err := s.repo.CreateOrUpdateSecret(ctx, secretNamespace, secretName, secrets); err != nil {
			return nil, fmt.Errorf("failed to store the credentials: %w", err)
		}
		// Tell consumers where the credentials are, so a package can bind them.
		public[valueSecretRef] = secretName
	}

	connection := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
			Labels: map[string]string{
				crd.LabelManagedBy: crd.ManagedByValue,
			},
		},
		Spec: crd.ConnectionSpec{
			// The type name IS the contract: a package asking for database-server
			// finds it by that name, here and in the catalog.
			Interface:   connectionType.Name,
			Description: req.Description,
			Values:      public,
		},
	}
	if len(secrets) > 0 {
		connection.Annotations = map[string]string{
			AnnotationCredentialsSecret: secretNamespace + "/" + secretName,
		}
	}

	if err := s.repo.Create(ctx, namespace, connection); err != nil {
		// Leave no orphan Secret behind when the Connection itself is refused.
		if len(secrets) > 0 {
			if cleanupErr := s.repo.DeleteSecret(ctx, secretNamespace, secretName); cleanupErr != nil {
				logrus.WithError(cleanupErr).Warn("Failed to clean up the credentials secret of a rejected connection")
			}
		}
		return nil, err
	}

	response := s.toResponse(connection, namespace)
	return &response, nil
}

func (s *DefaultConnectionService) Update(ctx context.Context, namespace, name string, req models.ConnectionRequest) (*models.ConnectionResponse, error) {
	req.Name = name
	connectionType, values, err := s.validateRequest(ctx, req, true)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if existing.IsManaged() {
		return nil, invalid("connection %q %s and cannot be edited", name, managedBy(existing))
	}

	public, secrets := splitValues(connectionType, values)
	secretNamespace := s.credentialsNamespace(namespace)
	secretName := name + credentialsSecretSuffix

	// An unchanged credential is not resubmitted by the console, so only write
	// the Secret when new values actually arrived — otherwise an edit of, say,
	// the port would blank out the password.
	if len(secrets) > 0 {
		if err := s.repo.CreateOrUpdateSecret(ctx, secretNamespace, secretName, secrets); err != nil {
			return nil, fmt.Errorf("failed to store the credentials: %w", err)
		}
		public[valueSecretRef] = secretName
	} else {
		// The credentials did not change, so neither do the fields describing
		// them — and they must be carried over: splitValues rebuilt the values
		// from the request, which no longer mentions them.
		for _, key := range []string{valueSecretRef} {
			if previous, ok := existing.Spec.Values[key]; ok {
				public[key] = previous
			}
		}
	}

	existing.Spec.Interface = connectionType.Name
	existing.Spec.Description = req.Description
	existing.Spec.Values = public

	if err := s.repo.Update(ctx, namespace, existing); err != nil {
		return nil, err
	}

	response := s.toResponse(existing, namespace)
	return &response, nil
}

func (s *DefaultConnectionService) Delete(ctx context.Context, namespace, name string) error {
	if !s.repo.Available(ctx) {
		return ErrConnectionsUnavailable
	}

	existing, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return err
	}
	if existing.IsManaged() {
		return invalid("connection %q %s and cannot be deleted; it exists as long as that service does", name, managedBy(existing))
	}

	if err := s.repo.Delete(ctx, namespace, name); err != nil {
		return err
	}

	// The Secret outlives the Connection only if this fails; report nothing to
	// the user for it, the connection itself is gone.
	if err := s.repo.DeleteSecret(ctx, s.credentialsNamespace(namespace), name+credentialsSecretSuffix); err != nil {
		logrus.WithError(err).Warn("Failed to delete the credentials secret of a removed connection")
	}
	return nil
}

func (s *DefaultConnectionService) Test(ctx context.Context, req models.ConnectionTestRequest) models.ConnectionTestResult {
	started := time.Now()

	result := func(success bool, reason, message string) models.ConnectionTestResult {
		return models.ConnectionTestResult{
			Success:    success,
			Reason:     reason,
			Message:    message,
			DurationMs: time.Since(started).Milliseconds(),
		}
	}

	if _, known := s.catalog.Get(req.Type); !known {
		return result(false, models.TestReasonInvalidConfig, fmt.Sprintf("Unknown connection type %q.", req.Type))
	}
	if err := s.catalog.Validate(req.Type, req.Values); err != nil {
		return result(false, models.TestReasonInvalidConfig, err.Error())
	}

	tester, ok := connectionTesters[req.Type]
	if !ok {
		return result(false, models.TestReasonInvalidConfig, "This connection type cannot be tested.")
	}

	if err := tester(ctx, req.Values); err != nil {
		var testErr *testFailure
		if errors.As(err, &testErr) {
			return result(false, testErr.reason, testErr.message)
		}
		return result(false, models.TestReasonUnknown, err.Error())
	}
	return result(true, "", "Connection successful.")
}

// --- Internal connections ---

func (s *DefaultConnectionService) ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error) {
	releases, err := s.releaseRepo.List(ctx, project, project)
	if err != nil {
		return nil, err
	}

	// Listing the namespace once and matching in memory keeps this to a single
	// API call whatever the number of deployed services.
	kubeServices, err := s.repo.ListKubeServices(ctx, project)
	if err != nil {
		logrus.WithError(err).Warn("Failed to list Kubernetes services; internal connections will have no endpoint")
		kubeServices = nil
	}

	result := make([]models.InternalConnection, 0)
	for i := range releases {
		release := &releases[i]
		serviceName := release.Labels[crd.LabelService]
		connectionType, provides := s.catalog.ForService(serviceName)
		if !provides {
			continue
		}
		result = append(result, s.deriveInternal(release, serviceName, connectionType, kubeServices, project))
	}

	// Once the CRDs are installed, the connections the release controller owns
	// are the authoritative description of what a service exposes; they replace
	// the entry derived for the same release.
	if s.repo.Available(ctx) {
		result = s.overlayManagedConnections(ctx, project, result)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *DefaultConnectionService) deriveInternal(
	release *crd.Release,
	serviceName string,
	connectionType *models.ConnectionType,
	kubeServices []corev1.Service,
	project string,
) models.InternalConnection {
	host, port := resolveEndpoint(kubeServices, release.Name, serviceName, connectionType.Providers, project)

	values := map[string]any{}
	if _, hasHost := connectionType.Field("host"); hasHost && host != "" {
		values["host"] = host
	}
	if _, hasPort := connectionType.Field("port"); hasPort && port != 0 {
		values["port"] = port
	}

	endpoint := ""
	if host != "" && port != 0 {
		endpoint = fmt.Sprintf("%s:%d", host, port)
	}

	createdAt := ""
	if ts := release.CreationTimestamp; !ts.IsZero() {
		createdAt = ts.Format(time.RFC3339)
	}

	return models.InternalConnection{
		Name:        release.Name,
		Type:        connectionType.Name,
		TypeDisplay: connectionType.DisplayName,
		Icon:        connectionType.Icon,
		Category:    connectionType.Category,
		Service:     serviceName,
		ReleaseName: release.Name,
		Namespace:   project,
		Status:      models.MapPhaseToStatus(release.Status.Phase),
		Endpoint:    endpoint,
		Host:        host,
		Port:        port,
		Values:      values,
		Managed:     false,
		Usage:       renderUsage(connectionType, values, nil),
		CreatedAt:   createdAt,
	}
}

func (s *DefaultConnectionService) overlayManagedConnections(
	ctx context.Context,
	project string,
	derived []models.InternalConnection,
) []models.InternalConnection {
	connections, err := s.repo.List(ctx, project)
	if err != nil {
		logrus.WithError(err).Warn("Failed to list managed connections; falling back to the derived ones")
		return derived
	}

	supersededRelease := map[string]bool{}
	var managed []models.InternalConnection
	for i := range connections {
		connection := &connections[i]
		if !connection.IsManaged() {
			continue
		}
		if connection.Status.Parent != "" {
			supersededRelease[connection.Status.Parent] = true
		}
		managed = append(managed, s.managedToInternal(connection, project))
	}
	if len(managed) == 0 {
		return derived
	}

	result := make([]models.InternalConnection, 0, len(derived)+len(managed))
	for _, entry := range derived {
		if !supersededRelease[entry.ReleaseName] {
			result = append(result, entry)
		}
	}
	return append(result, managed...)
}

func (s *DefaultConnectionService) managedToInternal(connection *crd.Connection, project string) models.InternalConnection {
	values := connection.Spec.Values
	if values == nil {
		values = map[string]any{}
	}

	entry := models.InternalConnection{
		Name:        connection.Name,
		Type:        connection.Spec.Interface,
		TypeDisplay: connection.Status.InterfaceDisplay,
		Service:     connection.Labels[crd.LabelService],
		ReleaseName: connection.Status.Parent,
		Namespace:   project,
		Status:      connection.Status.Phase,
		Values:      values,
		Managed:     true,
	}
	if connectionType, known := s.catalog.Get(connection.Spec.Interface); known {
		entry.Icon = connectionType.Icon
		entry.Category = connectionType.Category
		// The catalog's display name wins over what the controller wrote in
		// the status: KuboCD renders it as "[trino]", raw lookup markers
		// included, which is not a label for humans.
		entry.TypeDisplay = connectionType.DisplayName
		entry.Usage = renderUsage(connectionType, values, nil)
	}
	if entry.TypeDisplay == "" {
		entry.TypeDisplay = connection.Spec.Interface
	}

	if host, ok := values["host"].(string); ok {
		entry.Host = host
		if port, ok := toFloat(values["port"]); ok {
			entry.Port = int32(port)
			entry.Endpoint = fmt.Sprintf("%s:%d", host, entry.Port)
		}
	}
	if ts := connection.CreationTimestamp; !ts.IsZero() {
		entry.CreatedAt = ts.Format(time.RFC3339)
	}
	return entry
}

// resolveEndpoint finds the in-cluster address of the service backing a
// release. Nothing is guessed: an address is only returned when a Kubernetes
// Service actually carries it, so the console never shows an endpoint that
// would not resolve.
func resolveEndpoint(
	kubeServices []corev1.Service,
	releaseName, serviceName string,
	providers models.ConnectionProviders,
	namespace string,
) (string, int32) {
	best := -1
	bestScore := 0
	for i := range kubeServices {
		score := serviceScore(&kubeServices[i], releaseName, serviceName)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return "", 0
	}

	kubeService := &kubeServices[best]
	host := fmt.Sprintf("%s.%s.svc.cluster.local", kubeService.Name, namespace)
	return host, pickPort(kubeService.Spec.Ports, providers)
}

// serviceScore rates how likely a Service is to be the one exposing a release.
// A zero score excludes the Service.
func serviceScore(kubeService *corev1.Service, releaseName, serviceName string) int {
	// Headless and per-pod Services address individual replicas, not the
	// service as a whole.
	if kubeService.Spec.ClusterIP == corev1.ClusterIPNone {
		return 0
	}
	// Discovery/gossip ports of a cluster are not consumable endpoints.
	if strings.HasSuffix(kubeService.Name, "-headless") || strings.HasSuffix(kubeService.Name, "-worker") {
		return 0
	}

	name := kubeService.Name
	switch {
	case name == releaseName:
		return 4
	case strings.HasPrefix(name, releaseName+"-"):
		return 3
	case serviceName != "" && name == serviceName:
		return 2
	case serviceName != "" && strings.Contains(name, serviceName):
		return 1
	default:
		return 0
	}
}

// pickPort prefers the port names the connection type declares, in order, and
// falls back to the Service's first port.
func pickPort(ports []corev1.ServicePort, providers models.ConnectionProviders) int32 {
	for _, wanted := range providers.PortNames {
		for _, port := range ports {
			if port.Name == wanted {
				return port.Port
			}
		}
	}
	for _, port := range ports {
		if port.Port == providers.DefaultPort {
			return port.Port
		}
	}
	if len(ports) > 0 {
		return ports[0].Port
	}
	return providers.DefaultPort
}

// --- helpers ---

// validateRequest checks a submitted connection and returns the values as they
// should be stored: derived fields filled, fields put out of play by the
// submitted engine dropped. On an edit, credentials that were not resubmitted
// are kept as they are rather than reported as missing.
func (s *DefaultConnectionService) validateRequest(ctx context.Context, req models.ConnectionRequest, isUpdate bool) (*models.ConnectionType, map[string]any, error) {
	if !s.repo.Available(ctx) {
		return nil, nil, ErrConnectionsUnavailable
	}
	if errs := validation.IsDNS1123Subdomain(req.Name); len(errs) > 0 {
		return nil, nil, invalid("invalid connection name %q: %s", req.Name, errs[0])
	}

	connectionType, known := s.catalog.Get(req.Type)
	if !known {
		return nil, nil, invalid("unknown connection type %q", req.Type)
	}
	if !connectionType.External {
		return nil, nil, invalid("connections of type %q come from a deployed service and cannot be declared by hand", req.Type)
	}
	values := s.catalog.Normalize(req.Type, req.Values)
	validateValues := s.catalog.Validate
	if isUpdate {
		validateValues = s.catalog.ValidateUpdate
	}
	if err := validateValues(req.Type, values); err != nil {
		return nil, nil, invalid("%s", err.Error())
	}
	return connectionType, values, nil
}

// credentialsNamespace returns where the Secret of a connection lives. Platform
// connections are cluster-scoped and have no namespace of their own, so their
// credentials go to the platform namespace.
func (s *DefaultConnectionService) credentialsNamespace(namespace string) string {
	if namespace == "" {
		return s.platformNamespace
	}
	return namespace
}

// splitValues separates the values that go into the Connection spec from the
// credentials, which are stored in a Secret instead.
func splitValues(connectionType *models.ConnectionType, values map[string]any) (map[string]any, map[string][]byte) {
	public := map[string]any{}
	secrets := map[string][]byte{}

	for name, value := range values {
		field, known := connectionType.Field(name)
		if !known {
			continue
		}
		if field.Secret {
			if str, ok := value.(string); ok && str != "" {
				secrets[name] = []byte(str)
			}
			continue
		}
		public[name] = value
	}
	return public, secrets
}

func (s *DefaultConnectionService) toResponse(connection *crd.Connection, namespace string) models.ConnectionResponse {
	scope := models.ConnectionScopeProject
	if namespace == "" {
		scope = models.ConnectionScopePlatform
	}

	// The type name is the interface name: one type, one contract. The label
	// that used to carry the type separately is gone, and reading it would only
	// resurrect the names of types that no longer exist.
	connectionTypeName := connection.Spec.Interface

	values := connection.Spec.Values
	if values == nil {
		values = map[string]any{}
	}

	response := models.ConnectionResponse{
		Name:        connection.Name,
		Type:        connectionTypeName,
		Scope:       scope,
		Namespace:   namespace,
		Description: connection.Spec.Description,
		Status:      connection.Status.Phase,
		Message:     connection.Status.Message,
		Values:      values,
	}
	if connectionType, known := s.catalog.Get(connectionTypeName); known {
		response.SecretFields = connectionType.SecretFields()
		// Read the Secret back from the annotation rather than rebuilding the
		// name: a connection created before the convention, or edited by hand,
		// may point somewhere else, and a consumer needs the real reference.
		var secret *models.SecretRef
		if ref := connection.Annotations[AnnotationCredentialsSecret]; ref != "" {
			if ns, name, found := strings.Cut(ref, "/"); found {
				secret = &models.SecretRef{Name: name, Namespace: ns}
			}
		}
		if secret != nil && len(response.SecretFields) > 0 {
			response.CredentialsSecret = &models.CredentialsSecretRef{
				Name:      secret.Name,
				Namespace: secret.Namespace,
				Keys:      response.SecretFields,
			}
		}
		response.Usage = renderUsage(connectionType, values, secret)
	}
	if ts := connection.CreationTimestamp; !ts.IsZero() {
		response.CreatedAt = ts.Format(time.RFC3339)
	}
	return response
}

// managedBy names the release a connection belongs to, for the message shown
// when someone tries to edit or delete it. The controller fills status.parent
// only once it has reconciled, so a connection can legitimately be managed
// with no parent yet — saying `release ""` would read like a bug.
func managedBy(connection *crd.Connection) string {
	if parent := connection.Status.Parent; parent != "" {
		return fmt.Sprintf("is provided by the deployed service %q", parent)
	}
	return "is provided by a deployed service"
}

// IsNotFound reports whether an error from this service is a missing resource,
// so the handler can answer 404 without importing the Kubernetes error package.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

func (s *DefaultConnectionService) ListSelectable(ctx context.Context, project, iface string) ([]models.SelectableConnection, error) {
	// Without the CRDs there is nothing to bind; the form simply offers no
	// existing connection, which is accurate.
	if !s.repo.Available(ctx) {
		return []models.SelectableConnection{}, nil
	}

	result := []models.SelectableConnection{}
	collect := func(connections []crd.Connection, scope string) {
		for i := range connections {
			connection := &connections[i]
			if iface != "" && connection.Spec.Interface != iface {
				continue
			}
			if connection.Spec.Disabled {
				continue
			}
			result = append(result, models.SelectableConnection{
				Name:        connection.Name,
				Scope:       scope,
				Type:        connection.Spec.Interface,
				Status:      connection.Status.Phase,
				Description: connection.Spec.Description,
				Managed:     connection.IsManaged(),
				ProvidedBy:  connection.Status.Parent,
			})
		}
	}

	projectConnections, err := s.repo.List(ctx, project)
	if err != nil {
		return nil, err
	}
	collect(projectConnections, models.ConnectionScopeProject)

	// Platform-wide connections are shared by every project; a failure to list
	// them must not hide the project's own.
	if platformConnections, err := s.repo.List(ctx, ""); err == nil {
		collect(platformConnections, models.ConnectionScopePlatform)
	} else {
		logrus.WithError(err).Warn("Failed to list platform connections; offering project ones only")
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
