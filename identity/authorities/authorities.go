/*
 * Copyright 2017-2019 Kopano and its licensors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package authorities

import (
	"crypto"
	"errors"
	"fmt"
	"net/url"

	"github.com/dgrijalva/jwt-go"
	"stash.kopano.io/kgol/oidc-go"
)

// Details hold detail information about authorities identified by ID.
type Details struct {
	ID            string
	Name          string
	AuthorityType string

	ClientID     string
	ClientSecret string

	Insecure bool

	Scopes              []string
	ResponseType        string
	CodeChallengeMethod string

	registration AuthorityRegistration

	ready bool

	AuthorizationEndpoint *url.URL

	validationKeys map[string]crypto.PublicKey
}

// IsReady returns wether or not the associated registration entry was ready
// at time of creation of the associated details.
func (d *Details) IsReady() bool {
	return d.ready
}

// IdentityClaimValue returns the identity claim value from the provided data.
func (d *Details) IdentityClaimValue(claims interface{}) (string, error) {
	return d.registration.IdentityClaimValue(claims)
}

// JWTKeyfunc returns a key func to validate JWTs with the keys of the associated
// authority registration.
func (d *Details) JWTKeyfunc() jwt.Keyfunc {
	return d.validateJWT
}

func (d *Details) validateJWT(token *jwt.Token) (interface{}, error) {
	rawAlg, ok := token.Header[oidc.JWTHeaderAlg]
	if !ok {
		return nil, errors.New("no alg header")
	}
	alg, ok := rawAlg.(string)
	if !ok {
		return nil, errors.New("invalid alg value")
	}
	switch jwt.GetSigningMethod(alg).(type) {
	case *jwt.SigningMethodRSA:
	case *jwt.SigningMethodECDSA:
	case *jwt.SigningMethodRSAPSS:
	default:
		return nil, fmt.Errorf("unexpected alg value")
	}
	rawKid, ok := token.Header[oidc.JWTHeaderKeyID]
	if !ok {
		return nil, fmt.Errorf("no kid header")
	}
	kid, ok := rawKid.(string)
	if !ok {
		return nil, fmt.Errorf("invalid kid value")
	}

	if key, ok := d.validationKeys[kid]; ok {
		return key, nil
	}

	return nil, errors.New("no key available")
}
