package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/authgear/authgear-server/pkg/api"
	"github.com/authgear/authgear-server/pkg/api/apierrors"
	"github.com/authgear/authgear-server/pkg/api/event"
	"github.com/authgear/authgear-server/pkg/api/event/nonblocking"
	"github.com/authgear/authgear-server/pkg/api/model"
	"github.com/authgear/authgear-server/pkg/lib/app2app"
	"github.com/authgear/authgear-server/pkg/lib/authn/authenticationinfo"
	"github.com/authgear/authgear-server/pkg/lib/authn/challenge"
	identitybiometric "github.com/authgear/authgear-server/pkg/lib/authn/identity/biometric"
	"github.com/authgear/authgear-server/pkg/lib/authn/user"
	"github.com/authgear/authgear-server/pkg/lib/config"
	"github.com/authgear/authgear-server/pkg/lib/dpop"
	"github.com/authgear/authgear-server/pkg/lib/hook"
	"github.com/authgear/authgear-server/pkg/lib/interaction"
	interactionintents "github.com/authgear/authgear-server/pkg/lib/interaction/intents"
	"github.com/authgear/authgear-server/pkg/lib/interaction/nodes"
	"github.com/authgear/authgear-server/pkg/lib/oauth"
	"github.com/authgear/authgear-server/pkg/lib/oauth/oidc"
	"github.com/authgear/authgear-server/pkg/lib/oauth/protocol"
	"github.com/authgear/authgear-server/pkg/lib/session"
	"github.com/authgear/authgear-server/pkg/lib/session/access"
	"github.com/authgear/authgear-server/pkg/lib/session/idpsession"
	"github.com/authgear/authgear-server/pkg/lib/uiparam"
	"github.com/authgear/authgear-server/pkg/util/clock"
	"github.com/authgear/authgear-server/pkg/util/duration"
	"github.com/authgear/authgear-server/pkg/util/httputil"
	"github.com/authgear/authgear-server/pkg/util/jwkutil"
	"github.com/authgear/authgear-server/pkg/util/jwtutil"
	"github.com/authgear/authgear-server/pkg/util/log"
	"github.com/authgear/authgear-server/pkg/util/pkce"
	"github.com/authgear/authgear-server/pkg/util/slice"
)

//go:generate mockgen -source=handler_token.go -destination=handler_token_mock_test.go -package handler_test

const (
	AuthorizationCodeGrantType = "authorization_code"
	RefreshTokenGrantType      = "refresh_token"
	// nolint:gosec
	TokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"

	AnonymousRequestGrantType = "urn:authgear:params:oauth:grant-type:anonymous-request"
	BiometricRequestGrantType = "urn:authgear:params:oauth:grant-type:biometric-request"
	App2AppRequestGrantType   = "urn:authgear:params:oauth:grant-type:app2app-request"
	// nolint:gosec
	IDTokenGrantType        = "urn:authgear:params:oauth:grant-type:id-token"
	SettingsActionGrantType = "urn:authgear:params:oauth:grant-type:settings-action"
)

const (
	// nolint:gosec
	PreAuthenticatedURLTokenTokenType = "urn:authgear:params:oauth:token-type:pre-authenticated-url-token"
	// nolint:gosec
	IDTokenTokenType = "urn:ietf:params:oauth:token-type:id_token"
	// nolint:gosec
	DeviceSecretTokenType = "urn:x-oath:params:oauth:token-type:device-secret"
)

const AppSessionTokenDuration = duration.Short

// whitelistedGrantTypes is a list of grant types that would be always allowed
// to all clients.
var whitelistedGrantTypes = []string{
	AuthorizationCodeGrantType,
	RefreshTokenGrantType,
	AnonymousRequestGrantType,
	BiometricRequestGrantType,
	App2AppRequestGrantType,
	IDTokenGrantType,
	SettingsActionGrantType,
	TokenExchangeGrantType,
}

type IDTokenIssuer interface {
	Iss() string
	IssueIDToken(opts oidc.IssueIDTokenOptions) (token string, err error)
	VerifyIDToken(idToken string) (token jwt.Token, err error)
}

type AccessTokenIssuer interface {
	EncodeAccessToken(client *config.OAuthClientConfig, clientLike *oauth.ClientLike, grant *oauth.AccessGrant, userID string, token string) (string, error)
}

type EventService interface {
	DispatchEventOnCommit(payload event.Payload) error
}

type TokenHandlerUserFacade interface {
	GetRaw(id string) (*user.User, error)
}

type App2AppService interface {
	ParseTokenUnverified(requestJWT string) (t *app2app.Request, err error)
	ParseToken(requestJWT string, key jwk.Key) (*app2app.Request, error)
}

type ChallengeProvider interface {
	Consume(token string) (*challenge.Purpose, error)
}

type TokenHandlerLogger struct{ *log.Logger }

func NewTokenHandlerLogger(lf *log.Factory) TokenHandlerLogger {
	return TokenHandlerLogger{lf.New("oauth-token")}
}

type TokenHandlerCodeGrantStore interface {
	GetCodeGrant(codeHash string) (*oauth.CodeGrant, error)
	DeleteCodeGrant(*oauth.CodeGrant) error
}

type TokenHandlerSettingsActionGrantStore interface {
	GetSettingsActionGrant(codeHash string) (*oauth.SettingsActionGrant, error)
	DeleteSettingsActionGrant(*oauth.SettingsActionGrant) error
}

type TokenHandlerOfflineGrantStore interface {
	GetOfflineGrant(id string) (*oauth.OfflineGrant, error)
	DeleteOfflineGrant(*oauth.OfflineGrant) error

	AccessOfflineGrantAndUpdateDeviceInfo(id string, accessEvent access.Event, deviceInfo map[string]interface{}, expireAt time.Time) (*oauth.OfflineGrant, error)
	UpdateOfflineGrantAuthenticatedAt(id string, authenticatedAt time.Time, expireAt time.Time) (*oauth.OfflineGrant, error)
	UpdateOfflineGrantApp2AppDeviceKey(id string, newKey string, expireAt time.Time) (*oauth.OfflineGrant, error)
	UpdateOfflineGrantDeviceSecretHash(
		grantID string,
		newDeviceSecretHash string,
		dpopJKT string,
		expireAt time.Time) (*oauth.OfflineGrant, error)

	ListOfflineGrants(userID string) ([]*oauth.OfflineGrant, error)
	ListClientOfflineGrants(clientID string, userID string) ([]*oauth.OfflineGrant, error)
}

type TokenHandlerAppSessionTokenStore interface {
	CreateAppSessionToken(*oauth.AppSessionToken) error
}

type TokenHandlerOfflineGrantService interface {
	ComputeOfflineGrantExpiry(session *oauth.OfflineGrant) (expiry time.Time, err error)
}

type TokenHandlerTokenService interface {
	ParseRefreshToken(ctx context.Context, token string) (authz *oauth.Authorization, offlineGrant *oauth.OfflineGrant, tokenHash string, err error)
	IssueAccessGrant(
		client *config.OAuthClientConfig,
		scopes []string,
		authzID string,
		userID string,
		sessionID string,
		sessionKind oauth.GrantSessionKind,
		refreshTokenHash string,
		resp protocol.TokenResponse,
	) error
	IssueOfflineGrant(
		client *config.OAuthClientConfig,
		opts IssueOfflineGrantOptions,
		resp protocol.TokenResponse,
	) (offlineGrant *oauth.OfflineGrant, tokenHash string, err error)
	IssueRefreshTokenForOfflineGrant(
		offlineGrantID string,
		client *config.OAuthClientConfig,
		opts IssueOfflineGrantRefreshTokenOptions,
		resp protocol.TokenResponse,
	) (offlineGrant *oauth.OfflineGrant, tokenHash string, err error)
	IssueDeviceSecret(resp protocol.TokenResponse) (deviceSecretHash string)
}

type PreAuthenticatedURLTokenService interface {
	IssuePreAuthenticatedURLToken(
		options *IssuePreAuthenticatedURLTokenOptions,
	) (*IssuePreAuthenticatedURLTokenResult, error)
	ExchangeForAccessToken(
		client *config.OAuthClientConfig,
		sessionID string,
		token string,
	) (string, error)
}

type TokenHandler struct {
	Context                context.Context
	AppID                  config.AppID
	AppDomains             config.AppDomains
	HTTPProto              httputil.HTTPProto
	HTTPOrigin             httputil.HTTPOrigin
	OAuthFeatureConfig     *config.OAuthFeatureConfig
	IdentityFeatureConfig  *config.IdentityFeatureConfig
	OAuthClientCredentials *config.OAuthClientCredentials
	Logger                 TokenHandlerLogger

	Authorizations                  AuthorizationService
	CodeGrants                      TokenHandlerCodeGrantStore
	SettingsActionGrantStore        TokenHandlerSettingsActionGrantStore
	IDPSessions                     oauth.IDPSessionProvider
	OfflineGrants                   TokenHandlerOfflineGrantStore
	AppSessionTokens                TokenHandlerAppSessionTokenStore
	OfflineGrantService             TokenHandlerOfflineGrantService
	PreAuthenticatedURLTokenService PreAuthenticatedURLTokenService
	Graphs                          GraphService
	IDTokenIssuer                   IDTokenIssuer
	Clock                           clock.Clock
	TokenService                    TokenHandlerTokenService
	Events                          EventService
	SessionManager                  SessionManager
	App2App                         App2AppService
	Challenges                      ChallengeProvider
	CodeGrantService                CodeGrantService
	ClientResolver                  OAuthClientResolver
	UIInfoResolver                  UIInfoResolver

	RemoteIP        httputil.RemoteIP
	UserAgentString httputil.UserAgentString
	ValidateScopes  ScopesValidator
}

func (h *TokenHandler) Handle(rw http.ResponseWriter, req *http.Request, r protocol.TokenRequest) httputil.Result {
	client := resolveClient(h.ClientResolver, r)
	if client == nil {
		return tokenResultError{
			Response: protocol.NewErrorResponse("invalid_client", "invalid client ID"),
		}
	}

	result, err := h.doHandle(rw, req, client, r)
	if err != nil {
		var oauthError *protocol.OAuthProtocolError
		resultErr := tokenResultError{}
		if errors.As(err, &oauthError) {
			resultErr.StatusCode = oauthError.StatusCode
			resultErr.Response = oauthError.Response
		} else {
			h.Logger.WithError(err).Error("authz handler failed")
			resultErr.Response = protocol.NewErrorResponse("server_error", "internal server error")
			resultErr.InternalError = true
		}
		result = resultErr
	}

	return result
}

func (h *TokenHandler) doHandle(
	rw http.ResponseWriter,
	req *http.Request,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	if err := h.validateRequest(r, client); err != nil {
		return nil, err
	}

	allowedGrantTypes := client.GrantTypes
	allowedGrantTypes = append(allowedGrantTypes, whitelistedGrantTypes...)

	ok := false
	for _, grantType := range allowedGrantTypes {
		if r.GrantType() == grantType {
			ok = true
			break
		}
	}
	if !ok {
		return nil, protocol.NewError("unauthorized_client", "grant type is not allowed for this client")
	}

	switch r.GrantType() {
	case AuthorizationCodeGrantType:
		return h.handleAuthorizationCode(req.Context(), client, r)
	case RefreshTokenGrantType:
		resp, err := h.handleRefreshToken(req.Context(), client, r)
		if err != nil {
			return nil, err
		}
		return tokenResultOK{Response: resp}, nil
	case TokenExchangeGrantType:
		return h.handleTokenExchange(req.Context(), client, r)
	case AnonymousRequestGrantType:
		return h.handleAnonymousRequest(req.Context(), client, r)
	case BiometricRequestGrantType:
		return h.handleBiometricRequest(rw, req, client, r)
	case App2AppRequestGrantType:
		return h.handleApp2AppRequest(rw, req, client, h.OAuthFeatureConfig, r)
	case IDTokenGrantType:
		return h.handleIDToken(rw, req, client, r)
	case SettingsActionGrantType:
		return h.handleSettingsActionCode(client, r)
	default:
		panic("oauth: unexpected grant type")
	}
}

// nolint:gocognit
func (h *TokenHandler) validateRequest(r protocol.TokenRequest, client *config.OAuthClientConfig) error {
	switch r.GrantType() {
	case SettingsActionGrantType:
		fallthrough
	case AuthorizationCodeGrantType:
		if r.Code() == "" {
			return protocol.NewError("invalid_request", "code is required")
		}
		if client.IsPublic() {
			if r.CodeVerifier() == "" {
				return protocol.NewError("invalid_request", "PKCE code verifier is required")
			}
		}
		if client.IsConfidential() {
			if r.ClientSecret() == "" {
				return protocol.NewError("invalid_request", "client secret is required")
			}
		}
	case RefreshTokenGrantType:
		if r.RefreshToken() == "" {
			return protocol.NewError("invalid_request", "refresh token is required")
		}
	case AnonymousRequestGrantType:
		if r.JWT() == "" {
			return protocol.NewError("invalid_request", "jwt is required")
		}
	case BiometricRequestGrantType:
		if r.JWT() == "" {
			return protocol.NewError("invalid_request", "jwt is required")
		}
	case App2AppRequestGrantType:
		if r.JWT() == "" {
			return protocol.NewError("invalid_request", "jwt is required")
		}
		if r.RefreshToken() == "" {
			return protocol.NewError("invalid_request", "refresh token is required")
		}
		if r.ClientID() == "" {
			return protocol.NewError("invalid_request", "client id is required")
		}
		if r.RedirectURI() == "" {
			return protocol.NewError("invalid_request", "redirect uri is required")
		}
		if r.CodeChallenge() != "" && r.CodeChallengeMethod() != pkce.CodeChallengeMethodS256 {
			return protocol.NewError("invalid_request", "only 'S256' PKCE transform is supported")
		}
	case IDTokenGrantType:
		break
	case TokenExchangeGrantType:
		// The validation logics can be different depends on requested_token_type
		// Do the validation in methods for each requested_token_type
		break
	default:
		return protocol.NewError("unsupported_grant_type", "grant type is not supported")
	}

	return nil
}

var errInvalidAuthzCode = protocol.NewError("invalid_grant", "invalid authorization code")

func (h *TokenHandler) app2appVerifyAndConsumeChallenge(jwt string) (*app2app.Request, error) {
	app2appToken, err := h.App2App.ParseTokenUnverified(jwt)
	if err != nil {
		h.Logger.WithError(err).Debugln("invalid app2app jwt payload")
		return nil, protocol.NewError("invalid_request", "invalid app2app jwt payload")
	}
	purpose, err := h.Challenges.Consume(app2appToken.Challenge)
	if err != nil || *purpose != challenge.PurposeApp2AppRequest {
		h.Logger.WithError(err).Debugln("invalid app2app jwt challenge")
		return nil, protocol.NewError("invalid_request", "invalid app2app jwt challenge")
	}
	return app2appToken, nil
}

func (h *TokenHandler) app2appGetDeviceKeyJWKVerified(jwt string) (jwk.Key, error) {
	app2appToken, err := h.app2appVerifyAndConsumeChallenge(jwt)
	if err != nil {
		return nil, err
	}
	key := app2appToken.Key
	_, err = h.App2App.ParseToken(jwt, key)
	if err != nil {
		h.Logger.WithError(err).Debugln("invalid app2app jwt signature")
		return nil, protocol.NewError("invalid_request", "invalid app2app jwt signature")
	}
	return key, nil
}

func (h *TokenHandler) rotateDeviceSecret(
	ctx context.Context,
	offlineGrant *oauth.OfflineGrant,
	resp protocol.TokenResponse) (*oauth.OfflineGrant, error) {
	dpopJKT, _ := dpop.GetDPoPProofJKT(ctx)

	deviceSecretHash := h.TokenService.IssueDeviceSecret(resp)
	expiry, err := h.OfflineGrantService.ComputeOfflineGrantExpiry(offlineGrant)
	if err != nil {
		return nil, err
	}
	offlineGrant, err = h.OfflineGrants.UpdateOfflineGrantDeviceSecretHash(
		offlineGrant.ID,
		deviceSecretHash,
		dpopJKT,
		expiry,
	)
	if err != nil {
		return nil, err
	}
	return offlineGrant, nil
}

func (h *TokenHandler) rotateDeviceSecretIfDeviceSecretIsPresentAndValid(
	ctx context.Context,
	deviceSecret string,
	authorizedScopes []string,
	offlineGrant *oauth.OfflineGrant,
	resp protocol.TokenResponse,
) (*oauth.OfflineGrant, bool, error) {
	if deviceSecret == "" {
		// If device secret is not provided in the request, do not rotate
		return offlineGrant, false, nil
	}

	if subtle.ConstantTimeCompare([]byte(oauth.HashToken(deviceSecret)), []byte(offlineGrant.DeviceSecretHash)) != 1 {
		// If the provided device sercet is invalid, do not rotate
		return offlineGrant, false, nil
	}

	return h.rotateDeviceSecretIfSufficientScope(ctx, authorizedScopes, offlineGrant, resp)
}

func (h *TokenHandler) rotateDeviceSecretIfSufficientScope(
	ctx context.Context,
	authorizedScopes []string,
	offlineGrant *oauth.OfflineGrant,
	resp protocol.TokenResponse) (*oauth.OfflineGrant, bool, error) {
	if !oauth.ContainsAllScopes(authorizedScopes, []string{oauth.DeviceSSOScope}) {
		// No device secret, no rotation needed.
		return offlineGrant, false, nil
	}

	newOfflineGrant, err := h.rotateDeviceSecret(ctx, offlineGrant, resp)
	if err != nil {
		return nil, false, err
	}
	return newOfflineGrant, true, nil
}

func (h *TokenHandler) app2appUpdateDeviceKeyIfNeeded(
	client *config.OAuthClientConfig,
	offlineGrant *oauth.OfflineGrant,
	app2AppDeviceKey jwk.Key) (*oauth.OfflineGrant, error) {
	if app2AppDeviceKey != nil && client.App2appEnabled {
		newKeyJson, err := json.Marshal(app2AppDeviceKey)
		if err != nil {
			return nil, err
		}
		isSameKey := subtle.ConstantTimeCompare(newKeyJson, []byte(offlineGrant.App2AppDeviceKeyJWKJSON)) != 0
		if isSameKey {
			// If same key was provided, do nothing
		} else {
			if !client.App2appInsecureDeviceKeyBindingEnabled {
				return nil, protocol.NewError("invalid_request", "x_app2app_insecure_device_key_binding_enabled must be true to allow updating x_app2app_device_key_jwt")
			}
			if offlineGrant.App2AppDeviceKeyJWKJSON != "" {
				return nil, protocol.NewError("invalid_grant", "app2app device key cannot be changed")
			}
			expiry, err := h.OfflineGrantService.ComputeOfflineGrantExpiry(offlineGrant)
			if err != nil {
				return nil, err
			}
			newGrant, err := h.OfflineGrants.UpdateOfflineGrantApp2AppDeviceKey(offlineGrant.ID, string(newKeyJson), expiry)
			if err != nil {
				return nil, err
			}
			return newGrant, err
		}
	}
	return offlineGrant, nil
}

func (h *TokenHandler) handleAuthorizationCode(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	resp, err := h.IssueTokensForAuthorizationCode(ctx, client, r)
	if err != nil {
		return nil, err
	}

	return tokenResultOK{Response: resp}, nil
}

// nolint:gocognit
func (h *TokenHandler) IssueTokensForAuthorizationCode(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (protocol.TokenResponse, error) {
	deviceInfo, err := r.DeviceInfo()
	if err != nil {
		return nil, protocol.NewError("invalid_request", err.Error())
	}

	codeHash := oauth.HashToken(r.Code())
	codeGrant, err := h.CodeGrants.GetCodeGrant(codeHash)
	if errors.Is(err, oauth.ErrGrantNotFound) {
		return nil, errInvalidAuthzCode
	} else if err != nil {
		return nil, err
	}

	dpopProof := dpop.GetDPoPProof(ctx)
	if !codeGrant.MatchDPoPJKT(dpopProof) {
		return nil, ErrInvalidDPoPKeyBinding
	}

	// Restore uiparam
	uiInfo, _, err := h.UIInfoResolver.ResolveForAuthorizationEndpoint(client, codeGrant.AuthorizationRequest)
	if err != nil {
		return nil, err
	}

	uiParam := uiInfo.ToUIParam()
	// Restore uiparam into context.
	uiparam.WithUIParam(h.Context, &uiParam)

	if h.Clock.NowUTC().After(codeGrant.ExpireAt) {
		return nil, errInvalidAuthzCode
	}

	if codeGrant.RedirectURI != r.RedirectURI() {
		return nil, protocol.NewError("invalid_request", "invalid redirect URI")
	}

	// verify pkce
	needVerifyPKCE := client.IsPublic() || codeGrant.AuthorizationRequest.CodeChallenge() != "" || r.CodeVerifier() != ""
	if needVerifyPKCE {
		v, err := pkce.NewS256Verifier(r.CodeVerifier())
		if err != nil {
			return nil, errInvalidAuthzCode
		}
		if !v.Verify(codeGrant.AuthorizationRequest.CodeChallenge()) {
			return nil, errInvalidAuthzCode
		}
	}

	// verify client secret
	needClientSecret := client.IsConfidential()
	if needClientSecret {
		if r.ClientSecret() == "" {
			return nil, protocol.NewError("invalid_request", "invalid client secret")
		}
		credentialsItem, ok := h.OAuthClientCredentials.Lookup(client.ClientID)
		if !ok {
			return nil, protocol.NewError("invalid_request", "client secret is not supported for the client")
		}

		pass := false
		keys, _ := jwkutil.ExtractOctetKeys(credentialsItem.Set)
		for _, clientSecret := range keys {
			if subtle.ConstantTimeCompare([]byte(r.ClientSecret()), clientSecret) == 1 {
				pass = true
			}
		}
		if !pass {
			return nil, protocol.NewError("invalid_request", "invalid client secret")
		}
	}

	authz, err := h.Authorizations.GetByID(codeGrant.AuthorizationID)
	if errors.Is(err, oauth.ErrAuthorizationNotFound) {
		return nil, errInvalidAuthzCode
	} else if err != nil {
		return nil, err
	}

	resp, err := h.doIssueTokensForAuthorizationCode(ctx, client, codeGrant, authz, deviceInfo, r.App2AppDeviceKeyJWT())
	if err != nil {
		return nil, err
	}

	err = h.CodeGrants.DeleteCodeGrant(codeGrant)
	if err != nil {
		h.Logger.WithError(err).Error("failed to invalidate code grant")
	}

	return resp, nil
}

func (h *TokenHandler) handleRefreshToken(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (protocol.TokenResponse, error) {
	deviceInfo, err := r.DeviceInfo()
	if err != nil {
		return nil, protocol.NewError("invalid_request", err.Error())
	}

	authz, offlineGrant, refreshTokenHash, err := h.TokenService.ParseRefreshToken(ctx, r.RefreshToken())
	if err != nil {
		return nil, err
	}

	accessEvent := access.NewEvent(h.Clock.NowUTC(), h.RemoteIP, h.UserAgentString)
	offlineGrant.AccessInfo.LastAccess = accessEvent
	offlineGrantSession, ok := offlineGrant.ToSession(refreshTokenHash)
	if !ok {
		return nil, ErrInvalidRefreshToken
	}

	resp, err := h.issueTokensForRefreshToken(ctx, client, offlineGrantSession, authz)
	if err != nil {
		return nil, err
	}

	if client.ClientID != offlineGrantSession.ClientID {
		return nil, protocol.NewError("invalid_request", "client id doesn't match the refresh token")
	}

	expiry, err := h.OfflineGrantService.ComputeOfflineGrantExpiry(offlineGrant)
	if err != nil {
		return nil, err
	}

	_, err = h.OfflineGrants.AccessOfflineGrantAndUpdateDeviceInfo(offlineGrant.ID, accessEvent, deviceInfo, expiry)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (h *TokenHandler) handleTokenExchange(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	switch r.RequestedTokenType() {
	case PreAuthenticatedURLTokenTokenType:
		resp, err := h.handlePreAuthenticatedURLToken(ctx, client, r)
		if err != nil {
			return nil, err
		}
		return tokenResultOK{Response: resp}, nil
	default:
		// Note(tung): According to spec, requested_token_type is optional,
		// but we do not support it at the moment.
		return nil, protocol.NewError("invalid_request", "requested_token_type not supported")
	}
}

func (h *TokenHandler) resolveIDTokenSession(idToken jwt.Token) (sidSession session.ListableSession, ok bool, err error) {
	sidInterface, ok := idToken.Get(string(model.ClaimSID))
	if !ok {
		return nil, false, nil
	}

	sid, ok := sidInterface.(string)
	if !ok {
		return nil, false, nil
	}

	typ, sessionID, ok := oidc.DecodeSID(sid)
	if !ok {
		return nil, false, nil
	}

	switch typ {
	case session.TypeIdentityProvider:
		if sess, err := h.IDPSessions.Get(sessionID); err == nil {
			sidSession = sess
		}
	case session.TypeOfflineGrant:
		if sess, err := h.OfflineGrants.GetOfflineGrant(sessionID); err == nil {
			sidSession = sess
		}
	default:
		panic(fmt.Errorf("oauth: unknown session type: %v", typ))
	}

	return sidSession, true, nil
}

func (h *TokenHandler) verifyIDTokenDeviceSecretHash(ctx context.Context, offlineGrant *oauth.OfflineGrant, idToken jwt.Token, deviceSecret string) error {
	// Always do all checks to ensure this method consumes constant time
	var err error = nil
	deviceSecretHash := oauth.HashToken(deviceSecret)
	dsHashInterface, ok := idToken.Get(string(model.ClaimDeviceSecretHash))
	if !ok {
		err = protocol.NewError("invalid_grant", "expected ds_hash to be present in id token (subject_token)")
	}
	dsHash, ok := dsHashInterface.(string)
	if !ok {
		err = protocol.NewError("invalid_grant", "expected ds_hash to be a string")
	}
	if subtle.ConstantTimeCompare([]byte(dsHash), []byte(deviceSecretHash)) != 1 {
		err = protocol.NewError("invalid_grant", "the hash of device_secret (actor_token) does not match ds_hash in id token (subject_token)")
	}
	if subtle.ConstantTimeCompare([]byte(offlineGrant.DeviceSecretHash), []byte(deviceSecretHash)) != 1 {
		err = protocol.NewError("invalid_grant", "the device_secret (actor_token) does not bind to the session")
	}
	dpopProof := dpop.GetDPoPProof(ctx)
	if !offlineGrant.MatchDeviceSecretDPoPJKT(dpopProof) {
		err = ErrInvalidDPoPKeyBinding
	}
	return err
}

func (h *TokenHandler) handlePreAuthenticatedURLToken(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (protocol.TokenResponse, error) {
	if r.ActorTokenType() != DeviceSecretTokenType {
		return nil, protocol.NewError("invalid_request", fmt.Sprintf("expected actor_token_type = %v", DeviceSecretTokenType))
	}
	if r.SubjectTokenType() != IDTokenTokenType {
		return nil, protocol.NewError("invalid_request", fmt.Sprintf("expected subject_token_type = %v", IDTokenTokenType))
	}
	if r.ActorToken() == "" {
		return nil, protocol.NewError("invalid_request", "actor_token is required")
	}
	if r.SubjectToken() == "" {
		return nil, protocol.NewError("invalid_request", "subject_token is required")
	}

	deviceSecret := r.ActorToken()
	idToken, err := h.IDTokenIssuer.VerifyIDToken(r.SubjectToken())
	if err != nil {
		return nil, protocol.NewError("invalid_request", "subject_token is not a valid id token")
	}
	if r.Audience() != h.IDTokenIssuer.Iss() {
		return nil, protocol.NewError("invalid_request", fmt.Sprintf("expected audience to be %v", h.IDTokenIssuer.Iss()))
	}
	session, ok, err := h.resolveIDTokenSession(idToken)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, protocol.NewError("invalid_grant", "sid in id token (subject_token) is invalid")
	}

	var isAllowed bool = false
	var scopes []string
	var offlineGrant *oauth.OfflineGrant
	switch session := session.(type) {
	case *idpsession.IDPSession:
		return nil, protocol.NewError("invalid_grant", "invalid session type")
	case *oauth.OfflineGrant:
		offlineGrant = session
		isAllowed = offlineGrant.HasAllScopes(offlineGrant.InitialClientID, []string{oauth.PreAuthenticatedURLScope})
		scopes = offlineGrant.GetScopes(offlineGrant.InitialClientID)
	}
	if !isAllowed {
		return nil, protocol.NewError("insufficient_scope", "pre-authenticated url is not allowed for this session")
	}

	err = h.verifyIDTokenDeviceSecretHash(ctx, offlineGrant, idToken, deviceSecret)
	if err != nil {
		return nil, err
	}

	requestedScopes := r.Scope()
	if len(requestedScopes) > 0 {
		if !offlineGrant.HasAllScopes(offlineGrant.InitialClientID, requestedScopes) {
			return nil, protocol.NewError("invalid_scope", "requesting extra scopes is not allowed")
		}
		err = h.ValidateScopes(client, requestedScopes)
		if err != nil {
			return nil, err
		}
		scopes = requestedScopes
	}

	authz, err := h.Authorizations.CheckAndGrant(client.ClientID, offlineGrant.GetUserID(), scopes)
	if err != nil {
		return nil, err
	}

	options := &IssuePreAuthenticatedURLTokenOptions{
		AppID:           string(h.AppID),
		AuthorizationID: authz.ID,
		ClientID:        client.ClientID,
		OfflineGrantID:  offlineGrant.ID,
		Scopes:          scopes,
	}
	result, err := h.PreAuthenticatedURLTokenService.IssuePreAuthenticatedURLToken(options)
	if err != nil {
		return nil, err
	}

	resp := protocol.TokenResponse{}
	// Return the token in access_token as specified by RFC8963
	resp.AccessToken(result.Token)
	resp.TokenType(result.TokenType)
	resp.IssuedTokenType(PreAuthenticatedURLTokenTokenType)
	resp.ExpiresIn(result.ExpiresIn)

	offlineGrant, err = h.rotateDeviceSecret(
		ctx,
		offlineGrant,
		resp,
	)
	if err != nil {
		return nil, err
	}

	// Issue new id_token which associated to the new device_secret
	newIDToken, err := h.IDTokenIssuer.IssueIDToken(oidc.IssueIDTokenOptions{
		ClientID:           client.ClientID,
		SID:                oidc.EncodeSID(offlineGrant),
		AuthenticationInfo: offlineGrant.GetAuthenticationInfo(),
		// scopes are used for specifying which fields should be included in the ID token
		// those fields may include personal identifiable information
		// Since the ID token issued here will be used in id_token_hint
		// so no scopes are needed
		ClientLike:       oauth.ClientClientLike(client, []string{}),
		DeviceSecretHash: offlineGrant.DeviceSecretHash,
	})
	if err != nil {
		return nil, err
	}
	resp.IDToken(newIDToken)

	return resp, nil
}

type anonymousTokenInput struct {
	JWT string
}

func (i *anonymousTokenInput) GetAnonymousRequestToken() string {
	return i.JWT
}

func (i *anonymousTokenInput) SignUpAnonymousUserWithoutKey() bool {
	return false
}

func (i *anonymousTokenInput) GetPromotionCode() string { return "" }

var _ nodes.InputUseIdentityAnonymous = &anonymousTokenInput{}

func (h *TokenHandler) handleAnonymousRequest(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	if !client.HasFullAccessScope() {
		return nil, protocol.NewError(
			"unauthorized_client",
			"this client may not use anonymous user",
		)
	}

	deviceInfo, err := r.DeviceInfo()
	if err != nil {
		return nil, protocol.NewError("invalid_request", err.Error())
	}

	var graph *interaction.Graph
	err = h.Graphs.DryRun(interaction.ContextValues{}, func(ctx *interaction.Context) (*interaction.Graph, error) {
		var err error
		intent := &interactionintents.IntentAuthenticate{
			Kind:                     interactionintents.IntentAuthenticateKindLogin,
			SuppressIDPSessionCookie: true,
		}
		graph, err = h.Graphs.NewGraph(ctx, intent)
		if err != nil {
			return nil, err
		}

		var edges []interaction.Edge
		graph, edges, err = h.Graphs.Accept(ctx, graph, &anonymousTokenInput{
			JWT: r.JWT(),
		})
		if len(edges) != 0 {
			return nil, errors.New("interaction not completed for anonymous users")
		} else if err != nil {
			return nil, err
		}

		return graph, nil
	})

	if apierrors.IsKind(err, api.InvariantViolated) &&
		apierrors.AsAPIError(err).HasCause("AnonymousUserDisallowed") {
		return nil, protocol.NewError("unauthorized_client", "AnonymousUserDisallowed")
	} else if errors.Is(err, api.ErrInvalidCredentials) {
		return nil, protocol.NewError("invalid_grant", api.InvalidCredentials.Reason)
	} else if err != nil {
		return nil, err
	}

	info := authenticationinfo.T{
		UserID:          graph.MustGetUserID(),
		AuthenticatedAt: h.Clock.NowUTC(),
	}

	err = h.Graphs.Run(interaction.ContextValues{}, graph)
	if apierrors.IsAPIError(err) {
		return nil, protocol.NewError("invalid_request", err.Error())
	} else if err != nil {
		return nil, err
	}

	// TODO(oauth): allow specifying scopes
	scopes := []string{"openid", oauth.OfflineAccess, oauth.FullAccessScope}

	authz, err := h.Authorizations.CheckAndGrant(
		client.ClientID,
		info.UserID,
		scopes,
	)
	if err != nil {
		return nil, err
	}

	resp := protocol.TokenResponse{}

	issueDeviceToken := h.shouldIssueDeviceSecret(scopes)

	dpopJKT, _ := dpop.GetDPoPProofJKT(ctx)

	// SSOEnabled is false for refresh tokens that are granted by anonymous login
	opts := IssueOfflineGrantOptions{
		Scopes:             scopes,
		AuthorizationID:    authz.ID,
		AuthenticationInfo: info,
		DeviceInfo:         deviceInfo,
		SSOEnabled:         false,
		IssueDeviceSecret:  issueDeviceToken,
		DPoPJKT:            dpopJKT,
	}
	offlineGrant, tokenHash, err := h.issueOfflineGrant(
		client,
		authz.UserID,
		resp,
		opts,
		true)
	if err != nil {
		return nil, err
	}

	err = h.TokenService.IssueAccessGrant(client, scopes, authz.ID, authz.UserID,
		offlineGrant.ID, oauth.GrantSessionKindOffline, tokenHash, resp)
	if err != nil {
		err = h.translateAccessTokenError(err)
		return nil, err
	}

	if slice.ContainsString(scopes, "openid") {
		idToken, err := h.IDTokenIssuer.IssueIDToken(oidc.IssueIDTokenOptions{
			ClientID:           client.ClientID,
			SID:                oidc.EncodeSID(offlineGrant),
			AuthenticationInfo: offlineGrant.GetAuthenticationInfo(),
			ClientLike:         oauth.ClientClientLike(client, scopes),
			DeviceSecretHash:   offlineGrant.DeviceSecretHash,
		})
		if err != nil {
			return nil, err
		}
		resp.IDToken(idToken)
	}

	return tokenResultOK{Response: resp}, nil
}

func (h *TokenHandler) handleBiometricRequest(
	rw http.ResponseWriter,
	req *http.Request,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	if *h.IdentityFeatureConfig.Biometric.Disabled {
		return nil, protocol.NewError(
			"invalid_request",
			"biometric authentication is disabled",
		)
	}

	if !client.HasFullAccessScope() {
		return nil, protocol.NewError(
			"unauthorized_client",
			"this client may not use biometric authentication",
		)
	}
	_, payload, err := jwtutil.SplitWithoutVerify([]byte(r.JWT()))
	if err != nil {
		return nil, protocol.NewError("invalid_request", err.Error())
	}
	actionIface, _ := payload.Get("action")
	action, _ := actionIface.(string)
	switch action {
	case string(identitybiometric.RequestActionSetup):
		return h.handleBiometricSetup(req, client, r)
	case string(identitybiometric.RequestActionAuthenticate):
		return h.handleBiometricAuthenticate(req.Context(), client, r)
	default:
		return nil, protocol.NewError("invalid_request", fmt.Sprintf("invalid action: %v", actionIface))
	}
}

type biometricInput struct {
	JWT string
}

func (i *biometricInput) GetBiometricRequestToken() string {
	return i.JWT
}

func (h *TokenHandler) handleBiometricSetup(
	req *http.Request,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	s := session.GetSession(req.Context())
	if s == nil {
		return nil, protocol.NewErrorStatusCode("invalid_grant", "biometric setup requires authenticated user", http.StatusUnauthorized)
	}

	var graph *interaction.Graph
	err := h.Graphs.DryRun(interaction.ContextValues{}, func(ctx *interaction.Context) (*interaction.Graph, error) {
		var err error
		graph, err = h.Graphs.NewGraph(ctx, interactionintents.NewIntentAddIdentity(s.GetAuthenticationInfo().UserID))
		if err != nil {
			return nil, err
		}

		var edges []interaction.Edge
		graph, edges, err = h.Graphs.Accept(ctx, graph, &biometricInput{
			JWT: r.JWT(),
		})
		if len(edges) != 0 {
			return nil, errors.New("interaction no completed for biometric setup")
		} else if err != nil {
			return nil, err
		}

		return graph, nil
	})

	if apierrors.IsKind(err, api.InvariantViolated) &&
		apierrors.AsAPIError(err).HasCause("BiometricDisallowed") {
		return nil, protocol.NewError("unauthorized_client", "BiometricDisallowed")
	} else if apierrors.IsKind(err, api.InvariantViolated) &&
		apierrors.AsAPIError(err).HasCause("AnonymousUserAddIdentity") {
		return nil, protocol.NewError("unauthorized_client", "AnonymousUserAddIdentity")
	} else if errors.Is(err, api.ErrInvalidCredentials) {
		return nil, protocol.NewError("invalid_grant", api.InvalidCredentials.Reason)
	} else if err != nil {
		return nil, err
	}

	err = h.Graphs.Run(interaction.ContextValues{}, graph)
	if apierrors.IsAPIError(err) {
		return nil, protocol.NewError("invalid_request", err.Error())
	} else if err != nil {
		return nil, err
	}

	return tokenResultEmpty{}, nil
}

func (h *TokenHandler) handleBiometricAuthenticate(
	ctx context.Context,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	deviceInfo, err := r.DeviceInfo()
	if err != nil {
		return nil, protocol.NewError("invalid_request", err.Error())
	}

	var graph *interaction.Graph
	err = h.Graphs.DryRun(interaction.ContextValues{}, func(ctx *interaction.Context) (*interaction.Graph, error) {
		var err error
		intent := &interactionintents.IntentAuthenticate{
			Kind:                     interactionintents.IntentAuthenticateKindLogin,
			SuppressIDPSessionCookie: true,
		}
		graph, err = h.Graphs.NewGraph(ctx, intent)
		if err != nil {
			return nil, err
		}

		var edges []interaction.Edge
		graph, edges, err = h.Graphs.Accept(ctx, graph, &biometricInput{
			JWT: r.JWT(),
		})
		if len(edges) != 0 {
			return nil, errors.New("interaction no completed for biometric authenticate")
		} else if err != nil {
			return nil, err
		}

		return graph, nil
	})

	if apierrors.IsKind(err, api.InvariantViolated) &&
		apierrors.AsAPIError(err).HasCause("BiometricDisallowed") {
		return nil, protocol.NewError("unauthorized_client", "BiometricDisallowed")
	} else if errors.Is(err, api.ErrInvalidCredentials) {
		return nil, protocol.NewError("invalid_grant", api.InvalidCredentials.Reason)
	} else if err != nil {
		return nil, err
	}

	info := authenticationinfo.T{
		UserID:          graph.MustGetUserID(),
		AMR:             graph.GetAMR(),
		AuthenticatedAt: h.Clock.NowUTC(),
	}
	biometricIdentity := graph.MustGetUserLastIdentity()

	err = h.Graphs.Run(interaction.ContextValues{}, graph)
	if apierrors.IsAPIError(err) {
		return nil, protocol.NewError("invalid_request", err.Error())
	} else if err != nil {
		return nil, err
	}

	scopes := []string{"openid", oauth.OfflineAccess, oauth.FullAccessScope}
	requestedScopes := r.Scope()
	if len(requestedScopes) > 0 {
		err := h.ValidateScopes(client, requestedScopes)
		if err != nil {
			return nil, err
		}
		scopes = requestedScopes
	}

	if !oauth.ContainsAllScopes(scopes, []string{oauth.OfflineAccess, oauth.FullAccessScope}) {
		return nil, protocol.NewError("invalid_scope", "offline_access and full-access must be requested")
	}

	authz, err := h.Authorizations.CheckAndGrant(
		client.ClientID,
		info.UserID,
		scopes,
	)
	if err != nil {
		return nil, err
	}

	// Clean up any offline grants that were issued with the same identity.
	offlineGrants, err := h.OfflineGrants.ListOfflineGrants(authz.UserID)
	if err != nil {
		return nil, err
	}
	for _, offlineGrant := range offlineGrants {
		if offlineGrant.IdentityID == biometricIdentity.ID {
			err := h.OfflineGrants.DeleteOfflineGrant(offlineGrant)
			if err != nil {
				return nil, err
			}
		}
	}

	dpopJKT, _ := dpop.GetDPoPProofJKT(ctx)

	resp := protocol.TokenResponse{}

	issueDeviceToken := h.shouldIssueDeviceSecret(scopes)
	// SSOEnabled is false for refresh tokens that are granted by biometric login
	opts := IssueOfflineGrantOptions{
		Scopes:             scopes,
		AuthorizationID:    authz.ID,
		AuthenticationInfo: info,
		DeviceInfo:         deviceInfo,
		IdentityID:         biometricIdentity.ID,
		SSOEnabled:         false,
		IssueDeviceSecret:  issueDeviceToken,
		DPoPJKT:            dpopJKT,
	}
	offlineGrant, tokenHash, err := h.issueOfflineGrant(
		client,
		authz.UserID,
		resp,
		opts,
		true)
	if err != nil {
		return nil, err
	}

	err = h.TokenService.IssueAccessGrant(client, scopes, authz.ID, authz.UserID,
		offlineGrant.ID, oauth.GrantSessionKindOffline, tokenHash, resp)
	if err != nil {
		err = h.translateAccessTokenError(err)
		return nil, err
	}

	if slice.ContainsString(scopes, "openid") {
		idToken, err := h.IDTokenIssuer.IssueIDToken(oidc.IssueIDTokenOptions{
			ClientID:           client.ClientID,
			SID:                oidc.EncodeSID(offlineGrant),
			AuthenticationInfo: offlineGrant.GetAuthenticationInfo(),
			ClientLike:         oauth.ClientClientLike(client, scopes),
			DeviceSecretHash:   offlineGrant.DeviceSecretHash,
		})
		if err != nil {
			return nil, err
		}
		resp.IDToken(idToken)
	}

	// Biometric login should fire event user.authenticated
	// for other scenarios, ref: https://github.com/authgear/authgear-server/issues/2930
	userRef := model.UserRef{
		Meta: model.Meta{
			ID: authz.UserID,
		},
	}
	err = h.Events.DispatchEventOnCommit(&nonblocking.UserAuthenticatedEventPayload{
		UserRef:  userRef,
		Session:  *offlineGrant.ToAPIModel(),
		AdminAPI: false,
	})
	if err != nil {
		return nil, err
	}

	return tokenResultOK{Response: resp}, nil
}

func (h *TokenHandler) handleApp2AppRequest(
	rw http.ResponseWriter,
	req *http.Request,
	client *config.OAuthClientConfig,
	feature *config.OAuthFeatureConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	if !client.App2appEnabled {
		return nil, protocol.NewError(
			"unauthorized_client",
			"this client may not use app2app authentication",
		)
	}

	if !feature.Client.App2AppEnabled {
		return nil, protocol.NewError(
			"invalid_request",
			"app2app disabled",
		)
	}

	redirectURI, errResp := parseRedirectURI(client, h.HTTPProto, h.HTTPOrigin, h.AppDomains, []string{}, r)
	if errResp != nil {
		return nil, protocol.NewErrorWithErrorResponse(errResp)
	}

	_, originalOfflineGrant, refreshTokenHash, err := h.TokenService.ParseRefreshToken(req.Context(), r.RefreshToken())
	if err != nil {
		return nil, err
	}

	offlineGrantSession, ok := originalOfflineGrant.ToSession(refreshTokenHash)
	if !ok {
		return nil, ErrInvalidRefreshToken
	}

	// FIXME(DEV-1430): The new scopes should be validated against the new client
	scopes := offlineGrantSession.Scopes

	originalClient := h.ClientResolver.ResolveClient(offlineGrantSession.ClientID)
	if originalClient == nil {
		return nil, protocol.NewError("server_error", "cannot find original client for app2app")
	}

	app2appjwt := r.JWT()
	verifiedToken, err := h.app2appVerifyAndConsumeChallenge(app2appjwt)
	if err != nil {
		return nil, err
	}

	if originalOfflineGrant.App2AppDeviceKeyJWKJSON == "" {
		if !originalClient.App2appInsecureDeviceKeyBindingEnabled {
			return nil, protocol.NewError("invalid_grant", "app2app is not allowed in current session")
		}
		// The challenge must be verified in previous steps
		originalOfflineGrant, err = h.app2appUpdateDeviceKeyIfNeeded(originalClient, originalOfflineGrant, verifiedToken.Key)
		if err != nil {
			return nil, err
		}
	}

	parsedKey, err := jwk.ParseKey([]byte(originalOfflineGrant.App2AppDeviceKeyJWKJSON))
	if err != nil {
		return nil, err
	}
	_, err = h.App2App.ParseToken(app2appjwt, parsedKey)
	if err != nil {
		h.Logger.WithError(err).Debugln("invalid app2app jwt signature")
		return nil, protocol.NewError("invalid_request", "invalid app2app jwt signature")
	}

	authz, err := h.Authorizations.CheckAndGrant(
		client.ClientID,
		originalOfflineGrant.GetUserID(),
		scopes,
	)
	if err != nil {
		return nil, err
	}
	info := authenticationinfo.T{
		UserID:          originalOfflineGrant.GetUserID(),
		AuthenticatedAt: h.Clock.NowUTC(),
	}

	// FIXME(tung): It seems nonce is not needed in app2app because native apps are not using it?
	artificialAuthorizationRequest := make(protocol.AuthorizationRequest)
	artificialAuthorizationRequest["client_id"] = client.ClientID
	artificialAuthorizationRequest["scope"] = strings.Join(authz.Scopes, " ")
	artificialAuthorizationRequest["code_challenge"] = r.CodeChallenge()
	if originalOfflineGrant.SSOEnabled {
		artificialAuthorizationRequest["x_sso_enabled"] = "true"
	}

	originalIDPSessionID := originalOfflineGrant.IDPSessionID
	var sessionType session.Type = ""
	if originalIDPSessionID != "" {
		sessionType = session.TypeIdentityProvider
	}

	code, _, err := h.CodeGrantService.CreateCodeGrant(&CreateCodeGrantOptions{
		Authorization:        authz,
		SessionType:          sessionType,
		SessionID:            originalIDPSessionID,
		AuthenticationInfo:   info,
		IDTokenHintSID:       "",
		RedirectURI:          redirectURI.String(),
		AuthorizationRequest: artificialAuthorizationRequest,
		// App2app does not support DPoP
		// because the app which uses the code may not share the same storage with the app which issues the code
		DPoPJKT: "",
	})
	if err != nil {
		return nil, err
	}

	resp := protocol.TokenResponse{}
	resp.Code(code)
	return tokenResultOK{Response: resp}, nil
}

func (h *TokenHandler) handleIDToken(
	w http.ResponseWriter,
	req *http.Request,
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	if !client.HasFullAccessScope() {
		return nil, protocol.NewError(
			"unauthorized_client",
			"this client may not refresh id token",
		)
	}

	s := session.GetSession(req.Context())
	if s == nil {
		return nil, protocol.NewErrorStatusCode("invalid_grant", "valid session is required", http.StatusUnauthorized)
	}

	resp := protocol.TokenResponse{}

	var deviceSecretHash string
	offlineGrantSession, ok := s.(*oauth.OfflineGrantSession)
	if ok {
		offlineGrant, _, err := h.rotateDeviceSecretIfDeviceSecretIsPresentAndValid(
			req.Context(),
			r.DeviceSecret(),
			offlineGrantSession.Scopes,
			offlineGrantSession.OfflineGrant,
			resp,
		)
		if err != nil {
			return nil, err
		}
		deviceSecretHash = offlineGrant.DeviceSecretHash
	}
	idToken, err := h.IDTokenIssuer.IssueIDToken(oidc.IssueIDTokenOptions{
		ClientID:           client.ClientID,
		SID:                oidc.EncodeSID(s),
		AuthenticationInfo: s.GetAuthenticationInfo(),
		// scopes are used for specifying which fields should be included in the ID token
		// those fields may include personal identifiable information
		// Since the ID token issued here will be used in id_token_hint
		// so no scopes are needed
		ClientLike:       oauth.ClientClientLike(client, []string{}),
		DeviceSecretHash: deviceSecretHash,
	})
	if err != nil {
		return nil, err
	}
	resp.IDToken(idToken)
	return tokenResultOK{Response: resp}, nil
}

func (h *TokenHandler) revokeClientOfflineGrants(
	client *config.OAuthClientConfig,
	userID string) error {
	offlineGrants, err := h.OfflineGrants.ListClientOfflineGrants(client.ClientID, userID)
	if err != nil {
		return err
	}
	for _, offlineGrant := range offlineGrants {
		err := h.SessionManager.RevokeWithoutEvent(offlineGrant)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *TokenHandler) issueOfflineGrant(
	client *config.OAuthClientConfig,
	userID string,
	resp protocol.TokenResponse,
	opts IssueOfflineGrantOptions,
	revokeExistingGrants bool) (offlineGrant *oauth.OfflineGrant, tokenHash string, err error) {
	// First revoke existing refresh tokens if MaxConcurrentSession == 1
	if revokeExistingGrants && client.MaxConcurrentSession == 1 {
		err := h.revokeClientOfflineGrants(client, userID)
		if err != nil {
			return nil, "", err
		}
	}
	offlineGrant, tokenHash, err = h.TokenService.IssueOfflineGrant(client, opts, resp)
	if err != nil {
		return nil, "", err
	}
	return offlineGrant, tokenHash, nil
}

// nolint: gocognit
func (h *TokenHandler) doIssueTokensForAuthorizationCode(
	ctx context.Context,
	client *config.OAuthClientConfig,
	code *oauth.CodeGrant,
	authz *oauth.Authorization,
	deviceInfo map[string]interface{},
	app2appDeviceKeyJWT string,
) (protocol.TokenResponse, error) {
	issueRefreshToken := false
	issueIDToken := false
	issueDeviceToken := h.shouldIssueDeviceSecret(code.AuthorizationRequest.Scope())
	for _, scope := range code.AuthorizationRequest.Scope() {
		switch scope {
		case oauth.OfflineAccess:
			issueRefreshToken = true
		case "openid":
			issueIDToken = true
		}
	}

	if issueRefreshToken {
		// Only if client is allowed to use refresh tokens
		allowRefreshToken := false
		for _, grantType := range client.GrantTypes {
			if grantType == RefreshTokenGrantType {
				allowRefreshToken = true
				break
			}
		}
		if !allowRefreshToken {
			issueRefreshToken = false
		}
	}

	info := code.AuthenticationInfo

	var app2appDevicePublicKey jwk.Key = nil
	if app2appDeviceKeyJWT != "" && client.App2appEnabled {
		k, err := h.app2appGetDeviceKeyJWKVerified(app2appDeviceKeyJWT)
		if err != nil {
			return nil, err
		}
		app2appDevicePublicKey = k
	}

	scopes := code.AuthorizationRequest.Scope()

	resp := protocol.TokenResponse{}

	// Reauth
	// Update auth_time, app2app device key and device_secret of the offline grant if possible.
	if sid := code.IDTokenHintSID; sid != "" {
		if typ, sessionID, ok := oidc.DecodeSID(sid); ok && typ == session.TypeOfflineGrant {
			offlineGrant, err := h.OfflineGrants.GetOfflineGrant(sessionID)
			if err == nil {
				// Update auth_time
				if info.AuthenticatedAt.After(offlineGrant.AuthenticatedAt) {
					expiry, err := h.OfflineGrantService.ComputeOfflineGrantExpiry(offlineGrant)
					if err != nil {
						return nil, err
					}
					_, err = h.OfflineGrants.UpdateOfflineGrantAuthenticatedAt(offlineGrant.ID, info.AuthenticatedAt, expiry)
					if err != nil {
						return nil, err
					}
				}

				// Rotate device_secret
				offlineGrant, _, err = h.rotateDeviceSecretIfSufficientScope(ctx, scopes, offlineGrant, resp)
				if err != nil {
					return nil, err
				}

				// Update app2app device key
				if app2appDevicePublicKey != nil {
					_, err = h.app2appUpdateDeviceKeyIfNeeded(client, offlineGrant, app2appDevicePublicKey)
					if err != nil {
						return nil, err
					}
				}
			}
		}
	}

	dpopJKT, _ := dpop.GetDPoPProofJKT(ctx)

	// As required by the spec, we must include access_token.
	// If we issue refresh token, then access token is just the access token of the refresh token.
	// Else if id_token_hint is present, use the sid.
	// Otherwise we return an error.
	var accessTokenSessionID string
	var accessTokenSessionKind oauth.GrantSessionKind
	var refreshTokenHash string
	var deviceSecretHash string
	var sid string

	var offlineGrantIDPSessionID string
	switch session.Type(info.AuthenticatedBySessionType) {
	case session.TypeIdentityProvider:
		offlineGrantIDPSessionID = info.AuthenticatedBySessionID
	default:
		// no idp session id
	}

	opts := IssueOfflineGrantOptions{
		Scopes:             scopes,
		AuthorizationID:    authz.ID,
		AuthenticationInfo: info,
		IDPSessionID:       offlineGrantIDPSessionID,
		DeviceInfo:         deviceInfo,
		SSOEnabled:         code.AuthorizationRequest.SSOEnabled(),
		App2AppDeviceKey:   app2appDevicePublicKey,
		IssueDeviceSecret:  issueDeviceToken,
		DPoPJKT:            dpopJKT,
	}
	if issueRefreshToken {
		var offlineGrant *oauth.OfflineGrant
		var tokenHash string
		var err error
		switch session.Type(info.AuthenticatedBySessionType) {
		case session.TypeOfflineGrant:
			offlineGrant, tokenHash, err = h.TokenService.IssueRefreshTokenForOfflineGrant(
				info.AuthenticatedBySessionID,
				client,
				IssueOfflineGrantRefreshTokenOptions{
					Scopes:          scopes,
					AuthorizationID: authz.ID,
					DPoPJKT:         dpopJKT,
				}, resp)
			if err != nil {
				return nil, err
			}
		case session.TypeIdentityProvider:
			fallthrough
		default:
			offlineGrant, tokenHash, err = h.issueOfflineGrant(
				client,
				code.AuthenticationInfo.UserID,
				resp,
				opts,
				true)
			if err != nil {
				return nil, err
			}
		}

		sid = oidc.EncodeSID(offlineGrant)
		accessTokenSessionID = offlineGrant.ID
		accessTokenSessionKind = oauth.GrantSessionKindOffline
		refreshTokenHash = tokenHash
		deviceSecretHash = offlineGrant.DeviceSecretHash

		// ref: https://github.com/authgear/authgear-server/issues/2930
		if info.ShouldFireAuthenticatedEventWhenIssueOfflineGrant {
			userRef := model.UserRef{
				Meta: model.Meta{
					ID: authz.UserID,
				},
			}
			err = h.Events.DispatchEventOnCommit(&nonblocking.UserAuthenticatedEventPayload{
				UserRef:  userRef,
				Session:  *offlineGrant.ToAPIModel(),
				AdminAPI: false,
			})
			if err != nil {
				return nil, err
			}
		}
	} else if code.IDTokenHintSID != "" {
		sid = code.IDTokenHintSID
		if typ, sessionID, ok := oidc.DecodeSID(sid); ok {
			accessTokenSessionID = sessionID
			switch typ {
			case session.TypeOfflineGrant:
				accessTokenSessionKind = oauth.GrantSessionKindOffline
				offlineGrant, err := h.OfflineGrants.GetOfflineGrant(sessionID)
				if err != nil {
					return nil, err
				}
				// Include ds_hash in id_token if it exist
				deviceSecretHash = offlineGrant.DeviceSecretHash
			case session.TypeIdentityProvider:
				accessTokenSessionKind = oauth.GrantSessionKindSession
			default:
				panic(fmt.Errorf("unknown session type: %v", typ))
			}
		}
	} else if client.IsConfidential() {
		// allow issuing access tokens if scopes don't contain offline_access and the client is confidential
		// fill the response with nil for not returning the refresh token
		offlineGrant, _, err := h.issueOfflineGrant(
			client,
			code.AuthenticationInfo.UserID,
			nil,
			opts,
			false)
		if err != nil {
			return nil, err
		}
		sid = oidc.EncodeSID(offlineGrant)
		accessTokenSessionID = offlineGrant.ID
		accessTokenSessionKind = oauth.GrantSessionKindOffline
	}

	if accessTokenSessionID == "" || accessTokenSessionKind == "" {
		return nil, protocol.NewError("invalid_request", "cannot issue access token")
	}

	err := h.TokenService.IssueAccessGrant(
		client,
		code.AuthorizationRequest.Scope(),
		authz.ID,
		authz.UserID,
		accessTokenSessionID,
		accessTokenSessionKind,
		refreshTokenHash,
		resp)
	if err != nil {
		err = h.translateAccessTokenError(err)
		return nil, err
	}

	if issueIDToken {
		if sid == "" {
			return nil, protocol.NewError("invalid_request", "cannot issue ID token")
		}
		idToken, err := h.IDTokenIssuer.IssueIDToken(oidc.IssueIDTokenOptions{
			ClientID:           client.ClientID,
			SID:                sid,
			Nonce:              code.AuthorizationRequest.Nonce(),
			AuthenticationInfo: info,
			ClientLike:         oauth.ClientClientLike(client, code.AuthorizationRequest.Scope()),
			DeviceSecretHash:   deviceSecretHash,
		})
		if err != nil {
			return nil, err
		}
		resp.IDToken(idToken)
	}

	return resp, nil
}

func (h *TokenHandler) issueTokensForRefreshToken(
	ctx context.Context,
	client *config.OAuthClientConfig,
	offlineGrantSession *oauth.OfflineGrantSession,
	authz *oauth.Authorization,
) (protocol.TokenResponse, error) {
	issueIDToken := false
	for _, scope := range offlineGrantSession.Scopes {
		if scope == "openid" {
			issueIDToken = true
			break
		}
	}

	resp := protocol.TokenResponse{}

	offlineGrant, _, err := h.rotateDeviceSecretIfSufficientScope(
		ctx,
		offlineGrantSession.Scopes,
		offlineGrantSession.OfflineGrant,
		resp)
	if err != nil {
		return nil, err
	}

	if issueIDToken {
		idToken, err := h.IDTokenIssuer.IssueIDToken(oidc.IssueIDTokenOptions{
			ClientID:           client.ClientID,
			SID:                oidc.EncodeSID(offlineGrantSession.OfflineGrant),
			AuthenticationInfo: offlineGrantSession.GetAuthenticationInfo(),
			ClientLike:         oauth.ClientClientLike(client, authz.Scopes),
			DeviceSecretHash:   offlineGrant.DeviceSecretHash,
		})
		if err != nil {
			return nil, err
		}
		resp.IDToken(idToken)
	}

	err = h.TokenService.IssueAccessGrant(client, offlineGrantSession.Scopes,
		authz.ID, authz.UserID, offlineGrantSession.SessionID(),
		oauth.GrantSessionKindOffline, offlineGrantSession.TokenHash, resp)
	if err != nil {
		err = h.translateAccessTokenError(err)
		return nil, err
	}

	return resp, nil
}

func (h *TokenHandler) IssueAppSessionToken(ctx context.Context, refreshToken string) (string, *oauth.AppSessionToken, error) {
	authz, grant, refreshTokenHash, err := h.TokenService.ParseRefreshToken(ctx, refreshToken)
	if err != nil {
		return "", nil, err
	}

	// Ensure client is authorized with full user access (i.e. first-party client)
	if !authz.IsAuthorized([]string{oauth.FullAccessScope}) {
		return "", nil, protocol.NewError("access_denied", "the client is not authorized to have full user access")
	}

	now := h.Clock.NowUTC()
	token := oauth.GenerateToken()
	sToken := &oauth.AppSessionToken{
		AppID:            grant.AppID,
		OfflineGrantID:   grant.ID,
		CreatedAt:        now,
		ExpireAt:         now.Add(AppSessionTokenDuration),
		TokenHash:        oauth.HashToken(token),
		RefreshTokenHash: refreshTokenHash,
	}

	err = h.AppSessionTokens.CreateAppSessionToken(sToken)
	if err != nil {
		return "", nil, err
	}

	return token, sToken, err
}

func (h *TokenHandler) translateAccessTokenError(err error) error {
	if apiErr := apierrors.AsAPIError(err); apiErr != nil {
		if apiErr.Reason == hook.WebHookDisallowed.Reason {
			return protocol.NewError("server_error", "access token generation is disallowed by hook")
		}
	}

	return err
}

func (h *TokenHandler) shouldIssueDeviceSecret(scopes []string) bool {
	issueDeviceToken := false
	for _, scope := range scopes {
		switch scope {
		case oauth.DeviceSSOScope:
			issueDeviceToken = true
		}
	}
	return issueDeviceToken
}

func (h *TokenHandler) handleSettingsActionCode(
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (httputil.Result, error) {
	resp, err := h.IssueTokensForSettingsActionCode(client, r)
	if err != nil {
		return nil, err
	}

	return tokenResultOK{Response: resp}, nil
}

// nolint:gocognit
func (h *TokenHandler) IssueTokensForSettingsActionCode(
	client *config.OAuthClientConfig,
	r protocol.TokenRequest,
) (protocol.TokenResponse, error) {
	codeHash := oauth.HashToken(r.Code())
	settingsActionGrant, err := h.SettingsActionGrantStore.GetSettingsActionGrant(codeHash)
	if errors.Is(err, oauth.ErrGrantNotFound) {
		return nil, errInvalidAuthzCode
	} else if err != nil {
		return nil, err
	}

	// Restore uiparam
	uiInfo, _, err := h.UIInfoResolver.ResolveForAuthorizationEndpoint(client, settingsActionGrant.AuthorizationRequest)
	if err != nil {
		return nil, err
	}

	uiParam := uiInfo.ToUIParam()
	// Restore uiparam into context.
	uiparam.WithUIParam(h.Context, &uiParam)

	if h.Clock.NowUTC().After(settingsActionGrant.ExpireAt) {
		return nil, errInvalidAuthzCode
	}

	if settingsActionGrant.RedirectURI != r.RedirectURI() {
		return nil, protocol.NewError("invalid_request", "invalid redirect URI")
	}

	// verify pkce
	needVerifyPKCE := client.IsPublic() || settingsActionGrant.AuthorizationRequest.CodeChallenge() != "" || r.CodeVerifier() != ""
	if needVerifyPKCE {
		v, err := pkce.NewS256Verifier(r.CodeVerifier())
		if err != nil {
			return nil, errInvalidAuthzCode
		}
		if !v.Verify(settingsActionGrant.AuthorizationRequest.CodeChallenge()) {
			return nil, errInvalidAuthzCode
		}
	}

	// verify client secret
	needClientSecret := client.IsConfidential()
	if needClientSecret {
		if r.ClientSecret() == "" {
			return nil, protocol.NewError("invalid_request", "invalid client secret")
		}
		credentialsItem, ok := h.OAuthClientCredentials.Lookup(client.ClientID)
		if !ok {
			return nil, protocol.NewError("invalid_request", "client secret is not supported for the client")
		}

		pass := false
		keys, _ := jwkutil.ExtractOctetKeys(credentialsItem.Set)
		for _, clientSecret := range keys {
			if subtle.ConstantTimeCompare([]byte(r.ClientSecret()), clientSecret) == 1 {
				pass = true
			}
		}
		if !pass {
			return nil, protocol.NewError("invalid_request", "invalid client secret")
		}
	}

	err = h.SettingsActionGrantStore.DeleteSettingsActionGrant(settingsActionGrant)
	if err != nil {
		h.Logger.WithError(err).Error("failed to invalidate settings action grant")
	}

	return protocol.TokenResponse{}, nil
}
