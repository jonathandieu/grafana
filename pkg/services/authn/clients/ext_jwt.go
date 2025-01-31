package clients

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-jose/go-jose/v3/jwt"
	authlib "github.com/grafana/authlib/authn"
	"github.com/grafana/authlib/claims"

	"github.com/grafana/grafana/pkg/apimachinery/errutil"
	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/apiserver/endpoints/request"
	"github.com/grafana/grafana/pkg/services/authn"
	"github.com/grafana/grafana/pkg/services/login"
	"github.com/grafana/grafana/pkg/setting"
)

var _ authn.Client = new(ExtendedJWT)

const (
	ExtJWTAuthenticationHeaderName = "X-Access-Token"
	ExtJWTAuthorizationHeaderName  = "X-Grafana-Id"
)

var (
	errExtJWTInvalid = errutil.Unauthorized(
		"ext.jwt.invalid", errutil.WithPublicMessage("Failed to verify JWT"),
	)
	errExtJWTInvalidSubject = errutil.Unauthorized(
		"ext.jwt.invalid-subject", errutil.WithPublicMessage("Invalid token subject"),
	)
	errExtJWTMisMatchedNamespaceClaims = errutil.Unauthorized(
		"ext.jwt.namespace-mismatch", errutil.WithPublicMessage("Namespace claims didn't match between id token and access token"),
	)
	errExtJWTDisallowedNamespaceClaim = errutil.Unauthorized(
		"ext.jwt.namespace-disallowed", errutil.WithPublicMessage("Namespace claim doesn't allow access to requested namespace"),
	)
)

func ProvideExtendedJWT(cfg *setting.Cfg) *ExtendedJWT {
	keys := authlib.NewKeyRetriever(authlib.KeyRetrieverConfig{
		SigningKeysURL: cfg.ExtJWTAuth.JWKSUrl,
	})

	accessTokenVerifier := authlib.NewAccessTokenVerifier(authlib.VerifierConfig{
		AllowedAudiences: cfg.ExtJWTAuth.Audiences,
	}, keys)

	// For ID tokens, we explicitly do not validate audience, hence an empty AllowedAudiences
	// Namespace claim will be checked
	idTokenVerifier := authlib.NewIDTokenVerifier(authlib.VerifierConfig{}, keys)

	return &ExtendedJWT{
		cfg:                 cfg,
		log:                 log.New(authn.ClientExtendedJWT),
		namespaceMapper:     request.GetNamespaceMapper(cfg),
		accessTokenVerifier: accessTokenVerifier,
		idTokenVerifier:     idTokenVerifier,
	}
}

type ExtendedJWT struct {
	cfg                 *setting.Cfg
	log                 log.Logger
	accessTokenVerifier authlib.Verifier[authlib.AccessTokenClaims]
	idTokenVerifier     authlib.Verifier[authlib.IDTokenClaims]
	namespaceMapper     request.NamespaceMapper
}

func (s *ExtendedJWT) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
	jwtToken := s.retrieveAuthenticationToken(r.HTTPRequest)

	accessToken, err := s.accessTokenVerifier.Verify(ctx, jwtToken)
	if err != nil {
		return nil, errExtJWTInvalid.Errorf("failed to verify access token: %w", err)
	}

	accessTokenClaims := authlib.NewAccessClaims(*accessToken)

	idToken := s.retrieveAuthorizationToken(r.HTTPRequest)
	if idToken != "" {
		idTokenClaims, err := s.idTokenVerifier.Verify(ctx, idToken)
		if err != nil {
			return nil, errExtJWTInvalid.Errorf("failed to verify id token: %w", err)
		}

		return s.authenticateAsUser(authlib.NewIdentityClaims(*idTokenClaims), accessTokenClaims)
	}

	return s.authenticateAsService(accessTokenClaims)
}

func (s *ExtendedJWT) IsEnabled() bool {
	return s.cfg.ExtJWTAuth.Enabled
}

func (s *ExtendedJWT) authenticateAsUser(
	idTokenClaims claims.IdentityClaims,
	accessTokenClaims claims.AccessClaims,
) (*authn.Identity, error) {
	// Only allow id tokens signed for namespace configured for this instance.
	if allowedNamespace := s.namespaceMapper(s.getDefaultOrgID()); !claims.NamespaceMatches(idTokenClaims, allowedNamespace) {
		return nil, errExtJWTDisallowedNamespaceClaim.Errorf("unexpected id token namespace: %s", idTokenClaims.Namespace())
	}

	// Allow access tokens with either the same namespace as the validated id token namespace or wildcard (`*`).
	if !claims.NamespaceMatches(accessTokenClaims, idTokenClaims.Namespace()) {
		return nil, errExtJWTMisMatchedNamespaceClaims.Errorf("unexpected access token namespace: %s", accessTokenClaims.Namespace())
	}

	accessType, _, err := identity.ParseTypeAndID(accessTokenClaims.Subject())
	if err != nil {
		return nil, errExtJWTInvalidSubject.Errorf("unexpected identity: %s", accessTokenClaims.Subject())
	}

	if !claims.IsIdentityType(accessType, claims.TypeAccessPolicy) {
		return nil, errExtJWTInvalid.Errorf("unexpected identity: %s", accessTokenClaims.Subject())
	}

	t, id, err := identity.ParseTypeAndID(idTokenClaims.Subject())
	if err != nil {
		return nil, errExtJWTInvalid.Errorf("failed to parse id token subject: %w", err)
	}

	if !claims.IsIdentityType(t, claims.TypeUser) {
		return nil, errExtJWTInvalidSubject.Errorf("unexpected identity: %s", idTokenClaims.Subject())
	}

	// For use in service layer, allow higher privilege
	allowedKubernetesNamespace := accessTokenClaims.Namespace()
	if len(s.cfg.StackID) > 0 {
		// For single-tenant cloud use, choose the lower of the two (id token will always have the specific namespace)
		allowedKubernetesNamespace = idTokenClaims.Namespace()
	}

	return &authn.Identity{
		ID:                         id,
		Type:                       t,
		OrgID:                      s.getDefaultOrgID(),
		AuthenticatedBy:            login.ExtendedJWTModule,
		AuthID:                     accessTokenClaims.Subject(),
		AllowedKubernetesNamespace: allowedKubernetesNamespace,
		ClientParams: authn.ClientParams{
			SyncPermissions: true,
			FetchPermissionsParams: authn.FetchPermissionsParams{
				ActionsLookup: accessTokenClaims.DelegatedPermissions(),
			},
			FetchSyncedUser: true,
		}}, nil
}

func (s *ExtendedJWT) authenticateAsService(accessTokenClaims claims.AccessClaims) (*authn.Identity, error) {
	// Allow access tokens with that has a wildcard namespace or a namespace matching this instance.
	if allowedNamespace := s.namespaceMapper(s.getDefaultOrgID()); !claims.NamespaceMatches(accessTokenClaims, allowedNamespace) {
		return nil, errExtJWTDisallowedNamespaceClaim.Errorf("unexpected access token namespace: %s", accessTokenClaims.Namespace())
	}

	t, id, err := identity.ParseTypeAndID(accessTokenClaims.Subject())
	if err != nil {
		return nil, fmt.Errorf("failed to parse access token subject: %w", err)
	}

	if !claims.IsIdentityType(t, claims.TypeAccessPolicy) {
		return nil, errExtJWTInvalidSubject.Errorf("unexpected identity: %s", accessTokenClaims.Subject())
	}

	return &authn.Identity{
		ID:                         id,
		UID:                        id,
		Type:                       t,
		OrgID:                      s.getDefaultOrgID(),
		AuthenticatedBy:            login.ExtendedJWTModule,
		AuthID:                     accessTokenClaims.Subject(),
		AllowedKubernetesNamespace: accessTokenClaims.Namespace(),
		ClientParams: authn.ClientParams{
			SyncPermissions: true,
			FetchPermissionsParams: authn.FetchPermissionsParams{
				Roles: accessTokenClaims.Permissions(),
			},
			FetchSyncedUser: false,
		},
	}, nil
}

func (s *ExtendedJWT) Test(ctx context.Context, r *authn.Request) bool {
	if !s.cfg.ExtJWTAuth.Enabled {
		return false
	}

	rawToken := s.retrieveAuthenticationToken(r.HTTPRequest)
	if rawToken == "" {
		return false
	}

	parsedToken, err := jwt.ParseSigned(rawToken)
	if err != nil {
		return false
	}

	var claims jwt.Claims
	if err := parsedToken.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return false
	}

	return true
}

func (s *ExtendedJWT) Name() string {
	return authn.ClientExtendedJWT
}

func (s *ExtendedJWT) Priority() uint {
	// This client should come before the normal JWT client, because it is more specific, because of the Issuer check
	return 15
}

// retrieveAuthenticationToken retrieves the JWT token from the request.
func (s *ExtendedJWT) retrieveAuthenticationToken(httpRequest *http.Request) string {
	jwtToken := httpRequest.Header.Get(ExtJWTAuthenticationHeaderName)

	// Strip the 'Bearer' prefix if it exists.
	return strings.TrimPrefix(jwtToken, "Bearer ")
}

// retrieveAuthorizationToken retrieves the JWT token from the request.
func (s *ExtendedJWT) retrieveAuthorizationToken(httpRequest *http.Request) string {
	jwtToken := httpRequest.Header.Get(ExtJWTAuthorizationHeaderName)

	// Strip the 'Bearer' prefix if it exists.
	return strings.TrimPrefix(jwtToken, "Bearer ")
}

func (s *ExtendedJWT) getDefaultOrgID() int64 {
	orgID := int64(1)
	if s.cfg.AutoAssignOrg && s.cfg.AutoAssignOrgId > 0 {
		orgID = int64(s.cfg.AutoAssignOrgId)
	}
	return orgID
}
