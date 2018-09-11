/*
 * Copyright 2017 Kopano and its licensors
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

package backends

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orcaman/concurrent-map"
	"github.com/sirupsen/logrus"
	kcc "stash.kopano.io/kgol/kcc-go"

	"stash.kopano.io/kc/konnect"
	"stash.kopano.io/kc/konnect/config"
	kcDefinitions "stash.kopano.io/kc/konnect/identifier/backends/kc"
	"stash.kopano.io/kc/konnect/identity"
	"stash.kopano.io/kc/konnect/oidc"
)

const (
	kcSessionMaxRetries = 3
	kcSessionRetryDelay = 50 * time.Millisecond
)

var kcSupportedScopes = []string{
	oidc.ScopeProfile,
	oidc.ScopeEmail,
	konnect.ScopeID,
	konnect.ScopeRawSubject,
	kcDefinitions.ScopeKopanoGC,
}

// KCServerDefaultUsername is the default username used by KCIdentifierBackend
// for KCC when the provided username is empty.
var KCServerDefaultUsername = "SYSTEM"

// Property mappings for Kopano Server user meta data.
var (
	KCServerDefaultFamilyNameProperty = kcc.PR_SURNAME_A
	KCServerDefaultGivenNameProperty  = kcc.PR_GIVEN_NAME_A
)

// KCIdentifierBackend is a backend for the Identifier which connects to
// Kopano Core via kcc-go.
type KCIdentifierBackend struct {
	ctx context.Context

	c        *kcc.KCC
	username string
	password string

	globalSession      *kcc.Session
	globalSessionMutex sync.RWMutex
	useGlobalSession   bool
	sessions           cmap.ConcurrentMap

	logger logrus.FieldLogger
}

type kcUser struct {
	user *kcc.User
}

func (u *kcUser) Subject() string {
	return u.user.UserEntryID
}

func (u *kcUser) Email() string {
	return u.user.MailAddress
}

func (u *kcUser) EmailVerified() bool {
	return true
}

func (u *kcUser) Name() string {
	return u.user.FullName
}

func (u *kcUser) FamilyName() string {
	var n string
	if u.user.Props != nil {
		n, _ = u.user.Props.Get(KCServerDefaultFamilyNameProperty)
	} else {
		n = u.splitFullName()[1]
	}
	return n
}

func (u *kcUser) GivenName() string {
	var n string
	if u.user.Props != nil {
		n, _ = u.user.Props.Get(KCServerDefaultGivenNameProperty)
	} else {
		n = u.splitFullName()[0]
	}
	return n
}

func (u *kcUser) ID() int64 {
	return int64(u.user.ID)
}

func (u *kcUser) Username() string {
	return u.user.Username
}

func (u *kcUser) splitFullName() [2]string {
	// TODO(longsleep): Cache this, instead of doing every time.
	parts := strings.SplitN(u.user.FullName, " ", 2)
	if len(parts) == 2 {
		return [2]string{parts[0], parts[1]}
	}
	return [2]string{"", ""}
}

// NewKCIdentifierBackend creates a new KCIdentifierBackend with the provided
// parameters.
func NewKCIdentifierBackend(c *config.Config, client *kcc.KCC, username string, password string) (*KCIdentifierBackend, error) {
	b := &KCIdentifierBackend{
		c: client,

		logger: c.Logger,

		sessions: cmap.New(),
	}

	// Store credentials if given.
	if username != "" {
		b.username = username
		b.password = password
		b.useGlobalSession = true
	}

	b.logger.WithField("client", b.c.String()).Infoln("kc server identifier backend connection set up")

	return b, nil
}

// RunWithContext implements the Backend interface. KCIdentifierBackends keep
// a session to the accociated Kopano Core client. This session is auto renewed
// and auto rerestablished and is bound to the provided Context.
func (b *KCIdentifierBackend) RunWithContext(ctx context.Context) error {
	b.ctx = ctx

	// Helper to keep dedicated session running.
	if b.useGlobalSession {
		b.logger.WithField("username", b.username).Infoln("kc server identifier session enabled")

		go func() {
			retry := time.NewTimer(5 * time.Second)
			retry.Stop()
			refreshCh := make(chan bool, 1)
			for {
				b.setGlobalSession(nil)
				session, sessionErr := kcc.NewSession(ctx, b.c, b.username, b.password)
				if sessionErr != nil {
					b.logger.WithError(sessionErr).Errorln("failed to create kc server session")
					retry.Reset(5 * time.Second)
				} else {
					b.logger.Debugf("kc server identifier session established: %v", session)
					b.setGlobalSession(session)
					go func() {
						<-session.Context().Done()
						b.logger.Debugf("kc server identifier session has ended: %v", session)
						refreshCh <- true
					}()
				}

				select {
				case <-refreshCh:
					// will retry instantly.
				case <-retry.C:
					// will retry instantly.
				case <-ctx.Done():
					// exit.
					return
				}
			}
		}()
	}

	// Helper to clean out old session data from memory.
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				expired := make([]string, 0)
				for entry := range b.sessions.IterBuffered() {
					session := entry.Val.(*kcc.Session)
					if !session.IsActive() {
						expired = append(expired, entry.Key)
					}
				}
				for _, ref := range expired {
					b.sessions.Remove(ref)
				}
			case <-ctx.Done():
				// exit.
				return
			}
		}
	}()

	return nil
}

// Logon implements the Backend interface, enabling Logon with user name and
// password as provided. Requests are bound to the provided context.
func (b *KCIdentifierBackend) Logon(ctx context.Context, username, password string) (bool, *string, *string, error) {
	var logonFlags kcc.KCFlag
	logonFlags |= kcc.KOPANO_LOGON_NO_UID_AUTH

	response, err := b.c.Logon(ctx, username, password, logonFlags)
	if err != nil {
		return false, nil, nil, fmt.Errorf("kc identifier backend logon error: %v", err)
	}

	switch response.Er {
	case kcc.KCSuccess:
		// Session
		var session *kcc.Session
		var sessionRef string
		if response.SessionID != kcc.KCNoSessionID {
			sessionRef = response.SessionID.String() + "@" + response.ServerGUID
			if s, ok := b.sessions.Get(sessionRef); ok {
				session = s.(*kcc.Session)
				err = session.Refresh()
				if err != nil {
					return false, nil, nil, fmt.Errorf("kc identifier backend logon session error: %v", err)
				}
			} else {
				session, err = kcc.CreateSession(b.ctx, b.c, response.SessionID, response.ServerGUID, true)
				if err != nil {
					return false, nil, nil, fmt.Errorf("kc identifier backend logon session error: %v", err)
				}
			}
		} else {
			return false, nil, nil, fmt.Errorf("kc identifier backend logon missing session")
		}

		// Resolve user details.
		// TODO(longsleep): Avoid extra resolve when logon response already
		// includes the required data (TODO in core).
		resolve, err := b.resolveUsername(ctx, username, session)
		if err != nil {
			return false, nil, nil, fmt.Errorf("kc identifier backend logon resolve error: %v", err)
		}

		b.sessions.SetIfAbsent(sessionRef, session)
		b.logger.WithFields(logrus.Fields{
			"session":  session,
			"username": username,
			"id":       resolve.UserEntryID,
		}).Debugln("kc identifier backend logon")

		return true, &resolve.UserEntryID, &sessionRef, nil

	case kcc.KCERR_LOGON_FAILED:
		return false, nil, nil, nil
	}

	return false, nil, nil, fmt.Errorf("kc identifier backend logon failed: %v", response.Er)
}

// ResolveUser implements the Beckend interface, providing lookup for user by
// providing the username. Requests are bound to the provided context.
func (b *KCIdentifierBackend) ResolveUser(ctx context.Context, username string, sessionRef *string) (identity.UserWithUsername, error) {
	session, err := b.getSessionFromRef(ctx, sessionRef, true, true, false)
	if err != nil {
		return nil, fmt.Errorf("kc identifier backend resolve session error: %v", err)
	}

	response, err := b.resolveUsername(ctx, username, session)
	if err != nil {
		return nil, fmt.Errorf("kc identifier backend resolve user error: %v", err)
	}

	switch response.Er {
	case kcc.KCSuccess:
		// success.

		return &kcUser{
			user: &kcc.User{
				ID:          response.ID,
				Username:    username,
				UserEntryID: response.UserEntryID,
			},
		}, nil

	case kcc.KCERR_NOT_FOUND:
		return nil, nil
	}

	return nil, fmt.Errorf("kc identifier backend get user failed: %v", response.Er)
}

// GetUser implements the Backend interface, providing user meta data retrieval
// for the user specified by the userID. Requests are bound to the provided
// context.
func (b *KCIdentifierBackend) GetUser(ctx context.Context, userID string, sessionRef *string) (identity.User, error) {
	session, err := b.getSessionFromRef(ctx, sessionRef, true, true, false)
	if err != nil {
		return nil, fmt.Errorf("kc identifier backend resolve session error: %v", err)
	}

	response, err := b.getUser(ctx, userID, session)
	if err != nil {
		return nil, fmt.Errorf("kc identifier backend get user error: %v", err)
	}

	switch response.Er {
	case kcc.KCSuccess:
		// success.
		if response.User.UserEntryID != userID {
			return nil, fmt.Errorf("kc identifier backend get user returned wrong user")
		}

		return &kcUser{
			user: response.User,
		}, nil

	case kcc.KCERR_NOT_FOUND:
		return nil, nil
	}

	return nil, fmt.Errorf("kc identifier backend get user failed: %v", response.Er)
}

// RefreshSession implements the Backend interface providing refresh to KC session.
func (b *KCIdentifierBackend) RefreshSession(ctx context.Context, sessionRef *string) error {
	_, err := b.getSessionFromRef(ctx, sessionRef, true, true, false)
	return err
}

// DestroySession implements the Backend interface providing destroy to KC session.
func (b *KCIdentifierBackend) DestroySession(ctx context.Context, sessionRef *string) error {
	session, err := b.getSessionFromRef(ctx, sessionRef, false, false, true)
	if err != nil {
		return err
	}

	return session.Destroy(ctx, true)
}

// UserClaims implements the Backend interface, providing user specific claims
// for the user specified by the userID.
func (b *KCIdentifierBackend) UserClaims(userID string, authorizedScopes map[string]bool) map[string]interface{} {
	var claims map[string]interface{}

	if authorizedScope, _ := authorizedScopes[kcDefinitions.ScopeKopanoGC]; authorizedScope {
		claims = make(map[string]interface{})
		// Inject userID as ID claim.
		claims[kcDefinitions.KopanoGCIDClaim] = userID
	}

	return claims
}

// ScopesSupported implements the Backend interface, providing supported scopes
// when running this backend.
func (b *KCIdentifierBackend) ScopesSupported() []string {
	return kcSupportedScopes
}

func (b *KCIdentifierBackend) resolveUsername(ctx context.Context, username string, session *kcc.Session) (*kcc.ResolveUserResponse, error) {
	result, err := b.withSessionAndRetry(ctx, session, func(ctx context.Context, session *kcc.Session) (interface{}, error, bool) {
		user, err := b.c.ResolveUsername(ctx, username, session.ID())
		if err != nil {
			return nil, err, true
		}

		if user.Er == kcc.KCERR_NOT_FOUND {
			return nil, user.Er, false
		}

		return user, nil, true
	})
	if err != nil {
		return nil, err
	}

	user := result.(*kcc.ResolveUserResponse)
	return user, err
}

func (b *KCIdentifierBackend) getUser(ctx context.Context, userEntryID string, session *kcc.Session) (*kcc.GetUserResponse, error) {
	result, err := b.withSessionAndRetry(ctx, session, func(ctx context.Context, session *kcc.Session) (interface{}, error, bool) {
		user, err := b.c.GetUser(ctx, userEntryID, session.ID())
		if err != nil {
			return nil, err, true
		}

		if user.Er == kcc.KCERR_NOT_FOUND {
			return nil, user.Er, false
		}

		return user, nil, true
	})
	if err != nil {
		return nil, err
	}

	user := result.(*kcc.GetUserResponse)
	return user, err
}

func (b *KCIdentifierBackend) getSessionFromRef(ctx context.Context, sessionRef *string, register bool, refresh bool, removeIfRegistered bool) (*kcc.Session, error) {
	if b.useGlobalSession {
		return nil, nil
	}
	if sessionRef == nil {
		return nil, nil
	}

	var session *kcc.Session
	if s, ok := b.sessions.Get(*sessionRef); ok {
		// Existing session.
		session = s.(*kcc.Session)
		if refresh {
			// Refresh when requested to ensure it is still valid.
			err := session.Refresh()
			if err != nil {
				return nil, err
			}
		}
		if removeIfRegistered {
			b.sessions.Remove(*sessionRef)
		}
		return session, nil
	}

	// Recreate session from ref.
	sessionRefParts := strings.SplitN(*sessionRef, "@", 2)
	if len(sessionRefParts) != 2 || sessionRefParts[0] == "" || sessionRefParts[1] == "" {
		return nil, fmt.Errorf("invalid session ref")
	}
	sessionID, err := strconv.ParseUint(sessionRefParts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid session ref: %v", err)
	}
	session, err = kcc.CreateSession(b.ctx, b.c, kcc.KCSessionID(sessionID), sessionRefParts[1], true)
	if err != nil {
		return nil, err
	}
	if register {
		if ok := b.sessions.SetIfAbsent(*sessionRef, session); ok {
			b.logger.WithFields(logrus.Fields{
				"session": session,
			}).Debugln("kc identifier session register from ref")
		}
	}

	if refresh {
		// Refresh directly when requested.
		err = session.Refresh()
		if err != nil {
			return nil, err
		}
	}

	return session, nil
}

func (b *KCIdentifierBackend) withSessionAndRetry(ctx context.Context, session *kcc.Session, worker func(context.Context, *kcc.Session) (interface{}, error, bool)) (interface{}, error) {
	retries := 0
	for {
		if session == nil {
			// Maybe we have a global session to use?
			session = b.getGlobalSession()
		}
		if session == nil || !session.IsActive() {
			// So no session eh?
			return nil, fmt.Errorf("no server session")
		}

		var failedErr error
		for {
			result, err, shouldRetry := worker(ctx, session)
			if err != nil {
				if !shouldRetry {
					return result, err
				}

				failedErr = err
				break
			}

			// NOTE(longsleep): This is pretty crappy - is there a better way?
			kcErr := reflect.ValueOf(result).Elem().FieldByName("Er").Interface().(kcc.KCError)
			if kcErr != kcc.KCSuccess {
				if !shouldRetry {
					return result, kcErr
				}

				failedErr = kcErr
				break
			}

			return result, nil
		}

		if failedErr != nil {
			switch failedErr {
			case kcc.KCERR_END_OF_SESSION:
				session.Destroy(ctx, false)
			default:
				return nil, failedErr
			}
		}

		// If reach here, its a retry.
		select {
		case <-time.After(kcSessionRetryDelay):
			// Retry now.
		case <-ctx.Done():
			// Abort.
			return nil, ctx.Err()
		}

		retries++
		if retries > kcSessionMaxRetries {
			b.logger.WithField("retry", retries).Errorln("kc identifier backend giving up kc request")
			return nil, failedErr
		}
		b.logger.WithField("retry", retries).Debugln("kc identifier backend retry in progress")
	}
}

func (b *KCIdentifierBackend) setGlobalSession(session *kcc.Session) {
	b.globalSessionMutex.Lock()
	b.globalSession = session
	b.globalSessionMutex.Unlock()
}

func (b *KCIdentifierBackend) getGlobalSession() *kcc.Session {
	b.globalSessionMutex.RLock()
	session := b.globalSession
	b.globalSessionMutex.RUnlock()
	return session
}
