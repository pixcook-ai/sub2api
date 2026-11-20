package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestStoredResponseResourceRetrieveReturnsOnlyPublicJSON(t *testing.T) {
	db := storedResponseResourceDBForTest()
	r := storedResponseResourceRouterForTest(t, db, storedResponseResourceKeyForTest())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_local_0123456789abcdef0123456789abcdef", nil))
	if w.Code != http.StatusOK || w.Body.String() != string(db.record.PublicResponse) {
		t.Fatalf("retrieve=%d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("resource headers: %v", w.Header())
	}
	for _, private := range []string{"private-native-model", "private-binding", "private-lease", "AccountID", "ResolvedModel", "LeaseToken"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("private storage field leaked: %q", private)
		}
	}
	if db.userID != 1 || db.groupID != 3 || db.id != db.record.PublicID || db.getCalls != 1 {
		t.Fatalf("wrong resource owner scope: %+v", db)
	}
}

func TestStoredResponseResourcesRejectEveryRawQueryBeforeStorage(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, query := range []string{"model=allowed-model", "include=reasoning.encrypted_content", "foo=bar", "bad=%ZZ", "foo=1;bar=2"} {
			t.Run(method+"/"+query, func(t *testing.T) {
				db := storedResponseResourceDBForTest()
				r := storedResponseResourceRouterForTest(t, db, storedResponseResourceKeyForTest())
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(method, "/v1/responses/"+db.record.PublicID+"?"+query, nil))
				if w.Code != http.StatusBadRequest || db.getCalls != 0 || db.modelCalls != 0 || db.deleteCalls != 0 {
					t.Fatalf("query reached resource store: status=%d get=%d model=%d delete=%d", w.Code, db.getCalls, db.modelCalls, db.deleteCalls)
				}
			})
		}
	}
}

func TestStoredResponseResourcesRequireCurrentCompleteOwner(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, mutation := range []struct {
			name  string
			apply func(*service.APIKey) *service.APIKey
		}{
			{name: "missing key", apply: func(*service.APIKey) *service.APIKey { return nil }},
			{name: "missing user", apply: func(k *service.APIKey) *service.APIKey { k.UserID = 0; return k }},
			{name: "missing key ID", apply: func(k *service.APIKey) *service.APIKey { k.ID = 0; return k }},
			{name: "missing group ID", apply: func(k *service.APIKey) *service.APIKey { k.GroupID = nil; return k }},
			{name: "zero group ID", apply: func(k *service.APIKey) *service.APIKey { *k.GroupID = 0; return k }},
			{name: "missing current group", apply: func(k *service.APIKey) *service.APIKey { k.Group = nil; return k }},
			{name: "mismatched group", apply: func(k *service.APIKey) *service.APIKey { k.Group.ID++; return k }},
		} {
			t.Run(method+"/"+mutation.name, func(t *testing.T) {
				db := storedResponseResourceDBForTest()
				r := storedResponseResourceRouterForTest(t, db, mutation.apply(storedResponseResourceKeyForTest()))
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(method, "/v1/responses/"+db.record.PublicID, nil))
				if w.Code != http.StatusForbidden || db.getCalls != 0 || db.modelCalls != 0 || db.deleteCalls != 0 {
					t.Fatalf("incomplete owner reached resource store: status=%d get=%d model=%d delete=%d", w.Code, db.getCalls, db.modelCalls, db.deleteCalls)
				}
			})
		}
	}
}

func TestStoredResponseResourcesAuthorizeOriginalModelAgainstCurrentGroup(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			db := storedResponseResourceDBForTest()
			key := storedResponseResourceKeyForTest()
			key.Group.ModelAllowlist.Models = []string{db.record.ResolvedModel}
			r := storedResponseResourceRouterForTest(t, db, key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, "/v1/responses/"+db.record.PublicID, nil))
			if w.Code != http.StatusNotFound || db.deleteCalls != 0 || strings.Contains(w.Body.String(), "private-") {
				t.Fatalf("resolved model bypassed client model authorization: status=%d body=%s deletes=%d", w.Code, w.Body.String(), db.deleteCalls)
			}
		})
	}
}

func TestStoredResponseResourceDeleteAuthorizesTombstoneWithoutReadingDeletedContent(t *testing.T) {
	db := storedResponseResourceDBForTest()
	key := storedResponseResourceKeyForTest()
	r := storedResponseResourceRouterForTest(t, db, key)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/responses/"+db.record.PublicID, nil))
		var body struct {
			ID      string
			Object  string
			Deleted bool
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || body.ID != db.record.PublicID || body.Object != "response.deleted" || !body.Deleted {
			t.Fatalf("delete %d: status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	if db.getCalls != 0 || !reflect.DeepEqual(db.operations, []string{"model", "delete", "model", "delete"}) {
		t.Fatalf("DELETE must use retained management metadata: get=%d operations=%v", db.getCalls, db.operations)
	}
	// Revoking a model also revokes management of its retained tombstone.
	key.Group.ModelAllowlist.Models = []string{"another-model"}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/responses/"+db.record.PublicID, nil))
	if w.Code != http.StatusNotFound || db.deleteCalls != 2 {
		t.Fatalf("current model authorization ignored on repeated delete: status=%d deletes=%d", w.Code, db.deleteCalls)
	}
}

func TestStoredResponseResourcesReturnSafeStorageErrors(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, tc := range []struct {
			name   string
			err    error
			status int
		}{
			{name: "not found", err: service.ErrStoredResponseNotFound, status: 404},
			{name: "invalid", err: service.ErrStoredResponseInvalid, status: 400},
			{name: "unsupported", err: service.ErrStoredResponseUnsupported, status: 400},
			{name: "quota", err: service.ErrStoredResponseQuota, status: 429},
			{name: "too large", err: service.ErrStoredResponseTooLarge, status: 413},
			{name: "disabled", err: service.ErrStoredResponsesDisabled, status: 503},
			{name: "conflict", err: service.ErrStoredResponseConflict, status: 409},
			{name: "database private error", err: errors.New("private-database-query-and-credentials"), status: 503},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				db := storedResponseResourceDBForTest()
				if method == http.MethodGet {
					db.getErr = tc.err
				} else {
					db.deleteErr = tc.err
				}
				r := storedResponseResourceRouterForTest(t, db, storedResponseResourceKeyForTest())
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(method, "/v1/responses/"+db.record.PublicID, nil))
				if w.Code != tc.status || strings.Contains(w.Body.String(), "private-") {
					t.Fatalf("unsafe error: status=%d body=%s", w.Code, w.Body.String())
				}
				var envelope struct {
					Error struct {
						Message, Type string
						Code          any
					}
				}
				if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Message == "" || envelope.Error.Type == "" {
					t.Fatalf("invalid OpenAI error envelope: %s, %v", w.Body.String(), err)
				}
			})
		}
	}
}

func TestStoredResponseResourcesFailClosedOnMissingModelOrStorageRecord(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, model := range []string{"", " \t"} {
			t.Run(method+"/model="+model, func(t *testing.T) {
				db := storedResponseResourceDBForTest()
				db.record.Model = model
				r := storedResponseResourceRouterForTest(t, db, storedResponseResourceKeyForTest())
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(method, "/v1/responses/"+db.record.PublicID, nil))
				if w.Code != http.StatusServiceUnavailable || db.deleteCalls != 0 {
					t.Fatalf("empty model bypassed authorization: status=%d deletes=%d", w.Code, db.deleteCalls)
				}
			})
		}
	}
	db := storedResponseResourceDBForTest()
	id := db.record.PublicID
	db.record = nil
	r := storedResponseResourceRouterForTest(t, db, storedResponseResourceKeyForTest())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil storage record must fail closed: %d", w.Code)
	}
}

func storedResponseResourceRouterForTest(t *testing.T, db service.StoredResponseRepository, key *service.APIKey) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := service.DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, err := service.NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := NewStoredResponseHandler(svc)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if key != nil {
			c.Set(string(middleware.ContextKeyAPIKey), key)
		}
		c.Next()
	})
	r.GET("/v1/responses/:response_id", h.Retrieve)
	r.DELETE("/v1/responses/:response_id", h.Delete)
	return r
}

func storedResponseResourceKeyForTest() *service.APIKey {
	groupID := int64(3)
	return &service.APIKey{ID: 2, UserID: 1, GroupID: &groupID, Group: &service.Group{
		ID: groupID, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"allowed-model"}},
	}}
}

func storedResponseResourceDBForTest() *storedResponseResourceTestDB {
	return &storedResponseResourceTestDB{record: &service.StoredResponse{
		PublicID: "resp_local_0123456789abcdef0123456789abcdef", UserID: 1, APIKeyID: 99, GroupID: 3,
		Model: "allowed-model", ResolvedModel: "private-native-model", AccountID: 999,
		Binding: "private-binding", LeaseToken: "private-lease",
		PublicResponse: json.RawMessage("{\"id\":\"resp_local_0123456789abcdef0123456789abcdef\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"allowed-model\",\"output\":[]}"),
	}}
}

// This adapter checks handler protocol boundaries only. It does not stand in
// for actual shared authentication or PostgreSQL owner/tombstone acceptance.
type storedResponseResourceTestDB struct {
	service.StoredResponseRepository
	record                            *service.StoredResponse
	getErr, modelErr, deleteErr       error
	getCalls, modelCalls, deleteCalls int
	userID, groupID                   int64
	id                                string
	deleted                           bool
	operations                        []string
}

func (d *storedResponseResourceTestDB) GetForOwner(_ context.Context, userID, groupID int64, id string) (*service.StoredResponse, error) {
	d.getCalls++
	d.operations = append(d.operations, "get")
	d.userID, d.groupID, d.id = userID, groupID, id
	if d.deleted {
		return nil, service.ErrStoredResponseNotFound
	}
	return d.record, d.getErr
}

func (d *storedResponseResourceTestDB) GetManagementModel(_ context.Context, userID, groupID int64, id string) (string, error) {
	d.modelCalls++
	d.operations = append(d.operations, "model")
	d.userID, d.groupID, d.id = userID, groupID, id
	if d.modelErr != nil {
		return "", d.modelErr
	}
	return d.record.Model, nil
}

func (d *storedResponseResourceTestDB) Delete(_ context.Context, userID, groupID int64, id string) error {
	d.deleteCalls++
	d.operations = append(d.operations, "delete")
	d.userID, d.groupID, d.id = userID, groupID, id
	if d.deleteErr != nil {
		return d.deleteErr
	}
	d.deleted = true
	return nil
}
