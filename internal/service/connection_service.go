package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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

// AnnotationCredentialsOwned records whether the console wrote that Secret or
// merely points at one. Written at creation rather than derived later: reading
// the Secret back would cost one API call per connection in a list, and the
// name alone cannot tell, an external Secret being free to follow the same
// convention.
const AnnotationCredentialsOwned = "okdp.io/credentials-owned"

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

	// ListConsumers returns the services of a project bound to a connection,
	// read from what the release controller published rather than guessed.
	ListConsumers(ctx context.Context, project, name string) ([]models.ConnectionConsumer, error)
}

type DefaultConnectionService struct {
	repo        repository.ConnectionRepository
	releaseRepo repository.ServiceRepository
	catalog     ConnectionTypeCatalog
}

func NewDefaultConnectionService(
	repo repository.ConnectionRepository,
	releaseRepo repository.ServiceRepository,
	catalog ConnectionTypeCatalog,
) *DefaultConnectionService {
	return &DefaultConnectionService{
		repo:        repo,
		releaseRepo: releaseRepo,
		catalog:     catalog,
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
	connectionType, values, err := s.validateRequest(ctx, namespace, req, false)
	if err != nil {
		return nil, err
	}

	public, secrets := splitValues(connectionType, values)
	secretNamespace := namespace
	secretName := req.Name + credentialsSecretSuffix
	ownSecret := req.ExistingSecret == ""

	if !ownSecret {
		// Pointing at a Secret somebody else owns: nothing is written, and the
		// credential fields of the payload are ignored on purpose.
		secretName = req.ExistingSecret
		secrets = nil
		public[valueSecretRef] = secretName
	} else if len(secrets) > 0 {
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
	if len(secrets) > 0 || !ownSecret {
		connection.Annotations = map[string]string{
			AnnotationCredentialsSecret: secretNamespace + "/" + secretName,
			AnnotationCredentialsOwned:  strconv.FormatBool(ownSecret),
		}
	}

	if err := s.repo.Create(ctx, namespace, connection); err != nil {
		// Leave no orphan Secret behind when the Connection itself is refused.
		// A Secret we do not own is never touched.
		if ownSecret && len(secrets) > 0 {
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
	connectionType, values, err := s.validateRequest(ctx, namespace, req, true)
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
	secretNamespace := namespace
	secretName := name + credentialsSecretSuffix

	if req.ExistingSecret != "" {
		// Switched to, or kept on, a Secret owned elsewhere.
		public[valueSecretRef] = req.ExistingSecret
		existing.Annotations = withCredentialsSecret(existing.Annotations, secretNamespace+"/"+req.ExistingSecret, false)
	} else if len(secrets) > 0 {
		// An unchanged credential is not resubmitted by the console, so only
		// write the Secret when new values actually arrived, otherwise an edit
		// of, say, the port would blank out the password.
		if err := s.repo.CreateOrUpdateSecret(ctx, secretNamespace, secretName, secrets); err != nil {
			return nil, fmt.Errorf("failed to store the credentials: %w", err)
		}
		public[valueSecretRef] = secretName
		existing.Annotations = withCredentialsSecret(existing.Annotations, secretNamespace+"/"+secretName, true)
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

	// Only a Secret this server wrote is removed with the connection. One that
	// was already there, projected from a vault, belongs to whoever put it
	// there: deleting it would take the credentials of everything else reading
	// it. The name is no guide, an external Secret being free to follow the same
	// convention, so this reads what was recorded at creation.
	secretName, owned := credentialsSecretOf(existing, name)
	if !owned {
		logrus.WithField("secret", secretName).
			Info("Leaving the credentials secret in place, the connection did not own it")
		return nil
	}

	// The Secret outlives the Connection only if this fails; report nothing to
	// the user for it, the connection itself is gone.
	if err := s.repo.DeleteSecret(ctx, namespace, secretName); err != nil {
		logrus.WithError(err).Warn("Failed to delete the credentials secret of a removed connection")
	}
	return nil
}

// credentialsSecretOf returns the Secret holding a connection's credentials and
// whether this server wrote it.
func credentialsSecretOf(connection *crd.Connection, name string) (string, bool) {
	secretName := name + credentialsSecretSuffix
	if ref := connection.Annotations[AnnotationCredentialsSecret]; ref != "" {
		if _, referenced, found := strings.Cut(ref, "/"); found && referenced != "" {
			secretName = referenced
		}
	}
	return secretName, credentialsOwned(connection.Annotations, name, secretName)
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
	// Normalize first, exactly as Create and Update do: the form does not send a
	// derived field, so validating the raw payload would demand a JDBC driver
	// nobody was asked for. Testing the normalized values is also the point of
	// the button, which is to try what will actually be stored.
	values := s.catalog.Normalize(req.Type, req.Values)
	if err := s.catalog.Validate(req.Type, values); err != nil {
		return result(false, models.TestReasonInvalidConfig, err.Error())
	}

	tester, ok := connectionTesters[req.Type]
	if !ok {
		return result(false, models.TestReasonInvalidConfig, "This connection type cannot be tested.")
	}

	// A contract may publish several addresses. The verdict is the one a
	// workload will actually take; the other checks travel along for diagnosis,
	// because a store reachable publicly and unreachable in-cluster is exactly
	// the case a green test used to hide.
	checks := tester(ctx, values)
	outcome := result(true, "", "Connection successful.")
	outcome.Checks = checks
	for _, c := range checks {
		if !c.Decisive {
			continue
		}
		outcome.Success = c.Success
		outcome.Message = c.Message
		outcome.Reason = c.Reason
	}
	return outcome
}

// --- Internal connections ---

// ListInternal returns the connections the project's own services publish, and
// only those: a Connection the release controller owns, born from the `outputs`
// stanza of a package.
//
// The console used to add entries it fabricated itself, by matching a release's
// `okdp.io/service` label against a hardcoded list and guessing an address from
// the Kubernetes Service whose name looked closest. Those entries had no
// Connection behind them, so nothing could ever bind them: ListSelectable only
// offers real ones. They looked exactly like the real entries, and the tab
// invited the user to wire a service to them, which left copying an address by
// hand as the only way through. A service that publishes nothing is now absent
// rather than approximated.
func (s *DefaultConnectionService) ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error) {
	if !s.repo.Available(ctx) {
		return []models.InternalConnection{}, nil
	}

	connections, err := s.repo.List(ctx, project)
	if err != nil {
		return nil, err
	}

	result := make([]models.InternalConnection, 0, len(connections))
	for i := range connections {
		if !connections[i].IsManaged() {
			continue
		}
		result = append(result, s.managedToInternal(&connections[i], project))
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
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
	}
	if entry.TypeDisplay == "" {
		entry.TypeDisplay = connection.Spec.Interface
	}

	entry.Endpoint = endpointFrom(values, s.catalog, connection.Spec.Interface)
	// host and port stay filled when the contract has them, for the columns that
	// show them separately. Most contracts publish a URI instead.
	if host, ok := values["host"].(string); ok {
		entry.Host = host
		if port, ok := toFloat(values["port"]); ok {
			entry.Port = int32(port)
		}
	}
	if ts := connection.CreationTimestamp; !ts.IsZero() {
		entry.CreatedAt = ts.Format(time.RFC3339)
	}
	return entry
}

// --- helpers ---

// validateRequest checks a submitted connection and returns the values as they
// should be stored: derived fields filled, fields put out of play by the
// submitted engine dropped. On an edit, credentials that were not resubmitted
// are kept as they are rather than reported as missing.
func (s *DefaultConnectionService) validateRequest(ctx context.Context, namespace string, req models.ConnectionRequest, isUpdate bool) (*models.ConnectionType, map[string]any, error) {
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
	if req.ExistingSecret != "" {
		// The credentials live in a Secret we do not own, so the payload is not
		// expected to carry them: validate as an edit, which tolerates that.
		validateValues = s.catalog.ValidateUpdate
		if err := s.checkExistingSecret(ctx, namespace, req, connectionType); err != nil {
			return nil, nil, err
		}
	}
	if err := validateValues(req.Type, values); err != nil {
		return nil, nil, invalid("%s", err.Error())
	}
	return connectionType, values, nil
}

// checkExistingSecret refuses a connection pointed at a Secret that is absent or
// does not carry what the contract needs. Storing it anyway would produce a
// connection that looks healthy and whose consumers fail at pod start with
// CreateContainerConfigError, far from the form that caused it.
func (s *DefaultConnectionService) checkExistingSecret(
	ctx context.Context,
	namespace string,
	req models.ConnectionRequest,
	connectionType *models.ConnectionType,
) error {
	secretNamespace := namespace
	keys, found, err := s.repo.SecretKeys(ctx, secretNamespace, req.ExistingSecret)
	if err != nil {
		return fmt.Errorf("failed to read the secret %q: %w", req.ExistingSecret, err)
	}
	if !found {
		return invalid("no secret named %q in namespace %q", req.ExistingSecret, secretNamespace)
	}

	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		present[key] = true
	}
	var missing []string
	for _, field := range connectionType.SecretFields() {
		if !present[field] {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return invalid("secret %q does not carry the keys the %s contract needs: %s",
			req.ExistingSecret, connectionType.Name, strings.Join(missing, ", "))
	}
	return nil
}

// credentialsNamespace returns where the Secret of a connection lives. Platform
// connections are cluster-scoped and have no namespace of their own, so their
// credentials go to the platform namespace.
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
		Scope:       models.ConnectionScopeProject,
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
		if ref := connection.Annotations[AnnotationCredentialsSecret]; ref != "" && len(response.SecretFields) > 0 {
			if ns, name, found := strings.Cut(ref, "/"); found {
				response.CredentialsSecret = &models.CredentialsSecretRef{
					Name:      name,
					Namespace: ns,
					Keys:      response.SecretFields,
					Owned:     credentialsOwned(connection.Annotations, connection.Name, name),
				}
			}
		}
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
				Scope:       models.ConnectionScopeProject,
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

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// endpointFrom returns the address a reader can use to reach a connection.
//
// Which value carries it depends on the contract: database-server publishes a
// host and a port, trino a URI, hive a thrift URI, s3 a URL. Reading host+port
// and nothing else, as this did, meant a Ready trino, hive or s3 connection
// showed "not available yet" while its address sat in its own values, two
// clicks away in the details panel. The descriptor now names the keys that
// carry it, in order of preference.
func endpointFrom(values map[string]any, catalog ConnectionTypeCatalog, interfaceName string) string {
	if connectionType, known := catalog.Get(interfaceName); known {
		for _, key := range connectionType.EndpointFrom {
			if value, ok := values[key].(string); ok && value != "" {
				return value
			}
		}
	}
	// The host+port convention, kept as a fallback so a contract shaped like
	// database-server needs no declaration to be addressable.
	host, ok := values["host"].(string)
	if !ok || host == "" {
		return ""
	}
	if port, ok := toFloat(values["port"]); ok {
		return fmt.Sprintf("%s:%d", host, int32(port))
	}
	return host
}

// withCredentialsSecret records where a connection's credentials live, without
// dropping the other annotations.
func withCredentialsSecret(annotations map[string]string, reference string, owned bool) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationCredentialsSecret] = reference
	annotations[AnnotationCredentialsOwned] = strconv.FormatBool(owned)
	return annotations
}

// ListConsumers returns the services bound to a connection. The controller
// publishes both what a release watches and what it actually resolved, so this
// reads that rather than reparsing parameters and guessing.
func (s *DefaultConnectionService) ListConsumers(ctx context.Context, project, name string) ([]models.ConnectionConsumer, error) {
	releases, err := s.releaseRepo.List(ctx, project, project)
	if err != nil {
		return nil, err
	}

	consumers := make([]models.ConnectionConsumer, 0)
	for i := range releases {
		release := &releases[i]
		watched := referencesConnection(release.Status.WatchedInputConnections, project, name)
		effective := referencesConnection(release.Status.EffectiveInputConnections, project, name)
		if !watched && !effective {
			continue
		}
		consumers = append(consumers, models.ConnectionConsumer{
			Service:     strings.TrimPrefix(release.Name, project+"-"),
			ReleaseName: release.Name,
			Status:      release.Status.Phase,
			Effective:   effective,
		})
	}
	sort.Slice(consumers, func(i, j int) bool { return consumers[i].ReleaseName < consumers[j].ReleaseName })
	return consumers, nil
}

// referencesConnection reports whether one of the references points at the named
// connection of a project. A ClusterConnection carries no namespace of its own,
// so an empty namespace matches too: it is the platform-wide one of that name.
func referencesConnection(refs []crd.InputConnectionReference, project, name string) bool {
	for _, ref := range refs {
		if ref.Name == name && (ref.Namespace == project || ref.Namespace == "") {
			return true
		}
	}
	return false
}

// credentialsOwned reports whether the console wrote the credentials Secret.
// Connections created before the annotation existed fall back on the naming
// convention, which is what the console itself used to assume.
func credentialsOwned(annotations map[string]string, connectionName, secretName string) bool {
	if raw, present := annotations[AnnotationCredentialsOwned]; present {
		owned, err := strconv.ParseBool(raw)
		return err == nil && owned
	}
	return secretName == connectionName+credentialsSecretSuffix
}
