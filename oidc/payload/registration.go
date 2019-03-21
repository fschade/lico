/*
 * Copyright 2019 Kopano and its licensors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, version 3,
 * as published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package payload

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/dgrijalva/jwt-go"
	"github.com/mendsley/gojwk"

	"stash.kopano.io/kc/konnect/identity/clients"
	"stash.kopano.io/kc/konnect/oidc"
)

// ClientRegistrationRequest holds the incoming request data for the OpenID
// Connect Dynamic Client Registration 1.0 client registration endpoint as
// specified at https://openid.net/specs/openid-connect-registration-1_0.html#ClientRegistration and
// https://openid.net/specs/openid-connect-session-1_0.html#DynRegRegistrations
type ClientRegistrationRequest struct {
	RedirectURIs    []string `json:"redirect_uris"`
	ResponseTypes   []string `json:"response_types"`
	GrantTypes      []string `json:"grant_types"`
	ApplicationType string   `json:"application_type"`

	Contacts   []string `json:"contacts"`
	ClientName string   `json:"client_name"`
	ClientURI  string   `json:"client_uri"`

	RawJWKS json.RawMessage `json:"jwks"`

	RawIDTokenSignedResponseAlg    string `json:"id_token_signed_response_alg"`
	RawUserInfoSignedResponseAlg   string `json:"userinfo_signed_response_alg"`
	RawRequestObjectSigningAlg     string `json:"request_object_signing_alg"`
	RawTokenEndpointAuthMethod     string `json:"token_endpoint_auth_method"`
	RawTokenEndpointAuthSigningAlg string `json:"token_endpoint_auth_signing_alg"`

	PostLogoutRedirectURIs []string `json:"post_logout_redirect_uris"`

	JWKS *gojwk.Key `json:"-"`
}

// DecodeClientRegistrationRequest returns a ClientRegistrationRequest holding
// the provided request's data.
func DecodeClientRegistrationRequest(req *http.Request) (*ClientRegistrationRequest, error) {
	decoder := json.NewDecoder(req.Body)
	var crr ClientRegistrationRequest
	err := decoder.Decode(&crr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client registration request: %v", err)
	}

	if crr.RawJWKS != nil {
		jwks, err := gojwk.Unmarshal(crr.RawJWKS)
		if err != nil {
			return nil, fmt.Errorf("failed to decode client registration request jwks: %v", err)
		}
		// Only use keys.
		crr.JWKS = &gojwk.Key{
			Keys: jwks.Keys,
		}
	}

	return &crr, err
}

// Validate validates the request data of the accociated client registration
// request and fills in default data where required.
func (crr *ClientRegistrationRequest) Validate() error {
	if len(crr.RedirectURIs) == 0 {
		return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidRedirectURI, "redirect_uris required")
	}

	// Validate and filter response_type.
	if len(crr.ResponseTypes) == 0 {
		crr.ResponseTypes = []string{oidc.ResponseTypeCode}
	}
	requiredGrantTypes := make(map[string]bool)
	responseTypes := make([]string, 0)
	for _, responseType := range crr.ResponseTypes {
		switch responseType {
		case oidc.ResponseTypeCode:
			requiredGrantTypes[oidc.GrantTypeAuthorizationCode] = true
			responseTypes = append(responseTypes, responseType)
			// breaks

		case oidc.ResponseTypeCodeIDToken:
			fallthrough
		case oidc.ResponseTypeCodeIDTokenToken:
			fallthrough
		case oidc.ResponseTypeCodeToken:
			requiredGrantTypes[oidc.GrantTypeAuthorizationCode] = true
			requiredGrantTypes[oidc.GrantTypeImplicit] = true
			responseTypes = append(responseTypes, responseType)
			// breaks

		case oidc.ResponseTypeIDToken:
			fallthrough
		case oidc.ResponseTypeIDTokenToken:
			requiredGrantTypes[oidc.GrantTypeAuthorizationCode] = true
			requiredGrantTypes[oidc.GrantTypeImplicit] = true
			responseTypes = append(responseTypes, responseType)
			// breaks

		case oidc.ResponseTypeToken:
			responseTypes = append(responseTypes, responseType)

		default:
		}
	}
	crr.ResponseTypes = responseTypes

	// Filter and validate grant_types.
	if len(crr.GrantTypes) == 0 {
		crr.GrantTypes = []string{oidc.GrantTypeAuthorizationCode}
	}
	grantTypes := make([]string, 0)
	registeredGrantTypes := make(map[string]bool)
	for _, grantType := range crr.GrantTypes {
		switch grantType {
		case oidc.GrantTypeAuthorizationCode:
			fallthrough
		case oidc.GrantTypeImplicit:
			fallthrough
		case oidc.GrantTypeRefreshToken:
			registeredGrantTypes[grantType] = true
			grantTypes = append(grantTypes, grantType)
		default:
		}
	}
	for grantType := range requiredGrantTypes {
		if ok := registeredGrantTypes[grantType]; !ok {
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "grant_types conflict with response_types")
		}
	}

	if crr.ApplicationType == "" {
		crr.ApplicationType = oidc.ApplicationTypeWeb
	}
	switch crr.ApplicationType {
	case oidc.ApplicationTypeWeb:
		// Web Clients using the OAuth Implicit Grant Type MUST only register
		// URLs using the https scheme as redirect_uris; they MUST NOT use
		// localhost as the hostname.
		for _, uriString := range crr.RedirectURIs {
			uri, err := url.Parse(uriString)
			if err != nil {
				return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidRedirectURI, "failed to parse redirect_uris")
			}
			if ok := registeredGrantTypes[oidc.GrantTypeImplicit]; ok {
				if uri.Scheme != "https" {
					return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidRedirectURI, "implicit web clients must use https redirect_uris")
				}
				if uri.Hostname() == "localhost" {
					return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidRedirectURI, "implicit web clients must not use localhost redirect_uris")
				}
			}
		}

	case oidc.ApplicationTypeNative:
		// Native Clients MUST only register redirect_uris using custom URI
		// schemes or URLs using the http: scheme with localhost as the hostname.
		for _, uriString := range crr.RedirectURIs {
			uri, err := url.Parse(uriString)
			if err != nil {
				return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidRedirectURI, "failed to parse redirect_uris")
			}
			if uri.Scheme == "http" {
				if uri.Hostname() != "localhost" {
					return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidRedirectURI, "native clients must only use localhost redirect_uris with http")
				}
			}
		}

	default:
		return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "unknown application_type")
	}

	if crr.RawIDTokenSignedResponseAlg == "" {
		crr.RawIDTokenSignedResponseAlg = jwt.SigningMethodRS256.Alg()
	}
	if crr.RawIDTokenSignedResponseAlg != "" {
		alg := jwt.GetSigningMethod(crr.RawIDTokenSignedResponseAlg)
		if alg == nil {
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "unknown id_token_signed_response_alg")
		}
	}
	if crr.RawUserInfoSignedResponseAlg != "" {
		alg := jwt.GetSigningMethod(crr.RawUserInfoSignedResponseAlg)
		if alg == nil {
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "unknown userinfo_signed_response_alg")
		}
	}
	if crr.RawRequestObjectSigningAlg != "" {
		alg := jwt.GetSigningMethod(crr.RawRequestObjectSigningAlg)
		if alg == nil {
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "unknown request_object_signing_alg")
		}
	}
	if crr.RawTokenEndpointAuthMethod == "" {
		crr.RawTokenEndpointAuthMethod = oidc.AuthMethodClientSecretBasic
	}
	if crr.RawTokenEndpointAuthMethod != "" {
		switch crr.RawTokenEndpointAuthMethod {
		case oidc.AuthMethodClientSecretBasic:
			// breaks
		case oidc.AuthMethodNone:
			// breaks
		default:
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "unsupported token_endpoint_auth_method")
		}
	}
	if crr.RawTokenEndpointAuthSigningAlg != "" {
		alg := jwt.GetSigningMethod(crr.RawTokenEndpointAuthSigningAlg)
		if alg == nil {
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "unknown token_endpoint_auth_signing_alg")
		}
	}

	for _, uriString := range crr.PostLogoutRedirectURIs {
		_, err := url.Parse(uriString)
		if err != nil {
			return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "failed to parse post_logout_redirect_uris")
		}
	}

	if crr.JWKS != nil {
		if len(crr.JWKS.Keys) == 0 {
			crr.JWKS = nil
		} else {
			enc := false
			empty := true
			for _, key := range crr.JWKS.Keys {
				switch key.Use {
				case "":
					if enc {
						return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "jwks includes enc key and unset use key")
					}
					empty = true
					key.Use = "sig"
				case "enc":
					enc = true
					if empty {
						return oidc.NewOAuth2Error(oidc.ErrorOIDCInvalidClientMetadata, "jwks includes enc key and unset use key")
					}
				}
			}
		}
	}

	return nil
}

// ClientRegistration returns new dynamic client registration data for the
// accociated client registration request.
func (crr *ClientRegistrationRequest) ClientRegistration() (*clients.ClientRegistration, error) {
	cr := &clients.ClientRegistration{
		Contacts:        crr.Contacts,
		Name:            crr.ClientName,
		URI:             crr.ClientURI,
		GrantTypes:      crr.GrantTypes,
		ApplicationType: crr.ApplicationType,

		RedirectURIs: crr.RedirectURIs,

		JWKS: crr.JWKS,

		RawIDTokenSignedResponseAlg:    crr.RawIDTokenSignedResponseAlg,
		RawUserInfoSignedResponseAlg:   crr.RawUserInfoSignedResponseAlg,
		RawRequestObjectSigningAlg:     crr.RawRequestObjectSigningAlg,
		RawTokenEndpointAuthMethod:     crr.RawTokenEndpointAuthMethod,
		RawTokenEndpointAuthSigningAlg: crr.RawTokenEndpointAuthSigningAlg,

		PostLogoutRedirectURIs: crr.PostLogoutRedirectURIs,
	}
	err := cr.SetDynamic()
	if err != nil {
		return nil, err
	}

	return cr, nil
}

// ClientRegistrationResponse holds the outgoing data for a successful OpenID
// Connect Dynamic Client Registration 1.0 clientregistration request as
// specified at https://openid.net/specs/openid-connect-registration-1_0.html#RegistrationResponse
type ClientRegistrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`

	ClientIDIssuedAt      int64 `json:"client_id_issued_at,omitempty"`
	ClientSecretExpiresAt int64 `json:"client_secret_expires_at"`

	// Include validated request data.
	ClientRegistrationRequest
}
