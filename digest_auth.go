// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

type DigestAuth struct {
	Username string
	Password string
	Realm    string
	Expire   time.Duration

	// Algorithms are the digest hash algorithms advertised in the 401
	// challenge (RFC 8760). One WWW-Authenticate challenge is issued per
	// algorithm, each with its own nonce; the client picks one to respond
	// with. Must be supported by github.com/icholy/digest: MD5, SHA-256,
	// SHA-512, SHA-512-256. Defaults to MD5 when empty.
	Algorithms []string
}

// algorithms normalizes the advertised algorithm list. Empty means MD5,
// keeping RFC 2617-only clients working (pre-RFC 8760 default).
func (a *DigestAuth) algorithms() []string {
	if len(a.Algorithms) == 0 {
		return []string{"MD5"}
	}
	algs := make([]string, 0, len(a.Algorithms))
	for _, alg := range a.Algorithms {
		algs = append(algs, strings.ToUpper(alg))
	}
	return algs
}

func (a *DigestAuth) expire() time.Duration {
	if a.Expire > 0 {
		return a.Expire
	}
	return 5 * time.Second
}

type digestChallengeEntry struct {
	digest.Challenge
	expireTimer *time.Timer
}

type DigestAuthServer struct {
	mu    sync.Mutex
	cache map[string]*digestChallengeEntry
}

func NewDigestServer() *DigestAuthServer {
	t := &DigestAuthServer{
		cache: make(map[string]*digestChallengeEntry),
	}
	return t
}

func (s *DigestAuthServer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.cache {
		v.expireTimer.Stop()
	}
}

var (
	ErrDigestAuthNoChallenge = errors.New("no challenge")
	ErrDigestAuthBadCreds    = errors.New("bad credentials")
)

// AuthorizeRequest authorizes request. Returns SIP response that can be passed with error
func (s *DigestAuthServer) AuthorizeRequest(req *sip.Request, auth DigestAuth) (res *sip.Response, err error) {
	if auth.Realm == "" {
		auth.Realm = "sipgo"
	}

	h := req.GetHeader("Authorization")
	// https://www.rfc-editor.org/rfc/rfc2617#page-6

	if h == nil {
		return s.challenge(req, auth), nil
	}

	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		return sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil), err
	}

	s.mu.Lock()
	e, exists := s.cache[cred.Nonce]
	// Nonce is single-use: consume it atomically with the lookup. Deleting
	// only after a successful verification would leave a window where two
	// concurrent requests replaying the same nonce both authenticate.
	if exists {
		delete(s.cache, cred.Nonce)
	}
	s.mu.Unlock()
	if !exists {
		// Unknown or expired nonce: re-challenge so the client can retry.
		// We cannot distinguish expired from forged nonces (no nonce state),
		// so stale=true is not sent - clients treat a fresh challenge the same.
		return s.challenge(req, auth), ErrDigestAuthNoChallenge
	}
	e.expireTimer.Stop()
	chal := &e.Challenge

	// Make digest and compare response
	digCred, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: auth.Username,
		Password: auth.Password,
	})

	if err != nil {
		// Mostly due to unsupported digest alg
		return sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil), err
	}

	if cred.Response != digCred.Response {
		// Burn the presented nonce and answer with a fresh challenge: the
		// client can retry in the normal way, and the old nonce cannot be
		// reused for password guessing within its expiry window.
		s.mu.Lock()
		if stale, ok := s.cache[cred.Nonce]; ok {
			delete(s.cache, cred.Nonce)
			if stale.expireTimer != nil {
				stale.expireTimer.Stop()
			}
		}
		s.mu.Unlock()
		return s.challenge(req, auth), ErrDigestAuthBadCreds
	}

	return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil), nil
}

// challenge issues a fresh nonce per advertised algorithm and builds a 401
// carrying one WWW-Authenticate challenge per algorithm (RFC 8760)
func (s *DigestAuthServer) challenge(req *sip.Request, auth DigestAuth) *sip.Response {
	res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)

	for _, alg := range auth.algorithms() {
		nonce, err := generateNonce()
		if err != nil {
			return sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		}

		e := &digestChallengeEntry{
			Challenge: digest.Challenge{
				Realm: auth.Realm,
				Nonce: nonce,
				// Opaque:    "sipgo",
				Algorithm: alg,
			},
		}

		res.AppendHeader(sip.NewHeader("WWW-Authenticate", e.Challenge.String()))

		// Arm the expire timer before publishing the entry: any goroutine that
		// later reads the entry from the cache (AuthorizeRequest, Close) must
		// observe a non-nil expireTimer, otherwise it races on the field write.
		e.expireTimer = time.AfterFunc(auth.expire(), func() {
			s.mu.Lock()
			delete(s.cache, nonce)
			s.mu.Unlock()
		})
		s.mu.Lock()
		s.cache[nonce] = e
		s.mu.Unlock()
	}

	return res
}

func (s *DigestAuthServer) AuthorizeDialog(d *DialogServerSession, auth DigestAuth) error {
	// https://www.rfc-editor.org/rfc/rfc2617#page-6
	req := d.InviteRequest
	res, err := s.AuthorizeRequest(req, auth)
	if res != nil && res.StatusCode != sip.StatusOK {
		// Every non-2xx outcome must terminate the INVITE transaction here:
		// the initial challenge (err == nil), bad credentials, unknown or
		// expired nonces (fresh challenge), malformed credentials (400) and
		// digest errors (403). Without the write the caller hangs until the
		// framework falls through to Hangup and answers 480 instead.
		if err == nil {
			err = fmt.Errorf("not authorized")
		}
		return errors.Join(err, d.WriteResponse(res))
	}
	return errors.Join(err, nil)
}

func generateNonce() (string, error) {
	nonceBytes := make([]byte, 32)
	_, err := rand.Read(nonceBytes)
	if err != nil {
		return "", fmt.Errorf("could not generate nonce")
	}

	return base64.URLEncoding.EncodeToString(nonceBytes), nil
}
