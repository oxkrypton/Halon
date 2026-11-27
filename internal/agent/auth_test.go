package agent

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/incident"
)

func agentTokenFor(t *testing.T, signer *authz.Signer, role authz.Role) string {
	t.Helper()
	token, err := signer.Issue(authz.IssueOptions{
		Subject:   "human-1",
		Type:      authz.TypeHuman,
		Role:      role,
		Audiences: []authz.Audience{authz.AudienceAgent},
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newAuthTestRouter(t *testing.T, mode authz.Mode, keys map[string]ed25519.PublicKey, staticToken string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authenticator, err := authz.NewAuthenticator(authz.Config{
		Mode: mode, PublicKeys: keys, Issuer: authz.DefaultIssuer,
	}, authz.AudienceAgent, staticToken)
	if err != nil {
		t.Fatal(err)
	}
	return NewHTTPHandler(ServerDeps{
		Orchestrator: newTestOrchestrator(finalMessageModel("ok"), &stubReportStore{}, 8),
		Deduper:      incident.NewDeduper(time.Minute),
		Reports:      &stubReportReader{},
		Pods:         &stubPodLogReader{namespaces: []string{"default"}},
		IngestToken:  testIngestToken,
		Auth:         authenticator,
		Logger:       testLogger(),
	})
}

func getReports(router *gin.Engine, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestAgentAPIBRBAC(t *testing.T) {
	t.Parallel()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := authz.KeyID(public)
	signer, err := authz.NewSigner(private, kid, authz.DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{kid: public}
	jwtToken := agentTokenFor(t, signer, authz.RoleReadOnly)

	cases := []struct {
		name        string
		mode        authz.Mode
		staticToken string
		readToken   string
		wantRead    int
		ingestToken string
		wantIngest  int
	}{
		{"legacy ingest reads", authz.ModeLegacy, testIngestToken, testIngestToken, http.StatusOK, testIngestToken, http.StatusAccepted},
		{"hybrid compat reads", authz.ModeHybrid, testIngestToken, testIngestToken, http.StatusOK, testIngestToken, http.StatusAccepted},
		{"hybrid jwt reads", authz.ModeHybrid, testIngestToken, jwtToken, http.StatusOK, testIngestToken, http.StatusAccepted},
		{"jwt reads", authz.ModeJWT, "", jwtToken, http.StatusOK, testIngestToken, http.StatusAccepted},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			router := newAuthTestRouter(t, test.mode, keys, test.staticToken)
			if response := getReports(router, test.readToken); response.Code != test.wantRead {
				t.Fatalf("read status = %d, want %d; body = %s", response.Code, test.wantRead, response.Body.String())
			}
			response := postEvent(t, router, test.ingestToken, validEventBody())
			if response.Code != test.wantIngest {
				t.Fatalf("ingest status = %d, want %d; body = %s", response.Code, test.wantIngest, response.Body.String())
			}
		})
	}

	strict := newAuthTestRouter(t, authz.ModeJWT, keys, "")
	if response := getReports(strict, testIngestToken); response.Code != http.StatusUnauthorized {
		t.Fatalf("jwt mode accepted INGEST_TOKEN for reads: %d", response.Code)
	}
	if response := postEvent(t, strict, jwtToken, validEventBody()); response.Code != http.StatusUnauthorized {
		t.Fatalf("ingest accepted a human JWT: %d", response.Code)
	}

	// Pod log endpoints require k8s:pod_logs:read across legacy, hybrid, and jwt modes.
	pcrToken, err := signer.Issue(authz.IssueOptions{
		Subject:                "human-1",
		Type:                   authz.TypeHuman,
		Role:                   authz.RoleFull,
		PasswordChangeRequired: true,
		Audiences:              []authz.Audience{authz.AudienceAgent},
		TTL:                    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	doGet := func(r *gin.Engine, path, token string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, path := range []string{"/api/v1/k8s/pods", "/api/v1/k8s/pods/demo/logs"} {
		legacyRouter := newAuthTestRouter(t, authz.ModeLegacy, keys, testIngestToken)
		if got := doGet(legacyRouter, path, testIngestToken); got != http.StatusOK {
			t.Fatalf("legacy %s = %d, want 200", path, got)
		}
		hybridRouter := newAuthTestRouter(t, authz.ModeHybrid, keys, testIngestToken)
		if got := doGet(hybridRouter, path, testIngestToken); got != http.StatusOK {
			t.Fatalf("hybrid static %s = %d, want 200", path, got)
		}
		if got := doGet(hybridRouter, path, jwtToken); got != http.StatusOK {
			t.Fatalf("hybrid jwt %s = %d, want 200", path, got)
		}
		if got := doGet(strict, path, jwtToken); got != http.StatusOK {
			t.Fatalf("jwt %s = %d, want 200", path, got)
		}
		if got := doGet(strict, path, testIngestToken); got != http.StatusUnauthorized {
			t.Fatalf("jwt ingest token %s = %d, want 401", path, got)
		}
		if got := doGet(strict, path, pcrToken); got != http.StatusForbidden {
			t.Fatalf("jwt pcr token %s = %d, want 403", path, got)
		}
	}
}
