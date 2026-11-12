package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
)

// ResumeSigner creates and checks the signed resume URLs of waiting executions.
// A URL is bound to one Wait node of one execution and unguessable without
// storing per-execution tokens.
type ResumeSigner struct {
	key       []byte
	publicURL string
}

// NewResumeSigner derives its key from the server secret key.
func NewResumeSigner(secretKey, publicURL string) *ResumeSigner {
	sum := sha256.Sum256([]byte("g2f-resume-url\x00" + secretKey))
	return &ResumeSigner{key: sum[:], publicURL: strings.TrimRight(publicURL, "/")}
}

func (s *ResumeSigner) sign(execID, nodeID string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(execID + "\x00" + nodeID))
	return hex.EncodeToString(mac.Sum(nil))[:40]
}

// URL returns the URL that resumes the given Wait node of an execution.
func (s *ResumeSigner) URL(execID, nodeID string) string {
	return s.publicURL + "/webhook-resume/" + execID + "/" + url.PathEscape(nodeID) + "/" + s.sign(execID, nodeID)
}

func (s *ResumeSigner) valid(execID, nodeID, sig string) bool {
	return hmac.Equal([]byte(sig), []byte(s.sign(execID, nodeID)))
}

// Resumer continues waiting executions.
type Resumer interface {
	Resume(ctx context.Context, execID, nodeID string, item nodes.Item) (*store.Execution, error)
}

// ResumeHandler serves /webhook-resume/{executionId}/{nodeId}/{signature}
// for any method. The request (method, headers, query, body) becomes the Wait node's
// output, so e.g. ?decision=approve can drive a following Condition node.
type ResumeHandler struct {
	signer *ResumeSigner
	eng    Resumer
	log    *slog.Logger
}

func NewResumeHandler(signer *ResumeSigner, eng Resumer, log *slog.Logger) *ResumeHandler {
	return &ResumeHandler{signer: signer, eng: eng, log: log.With("component", "resume")}
}

func (h *ResumeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Split the escaped path so a node ID containing "/" stays one segment.
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/webhook-resume/"), "/"), "/")
	if len(parts) != 3 {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	nodeID, err := url.PathUnescape(parts[1])
	if err != nil || !h.signer.valid(parts[0], nodeID, parts[2]) {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	item := requestItem(r, body, &nodes.Webhook{})
	exec, err := h.eng.Resume(r.Context(), parts[0], nodeID, item)
	if errors.Is(err, engine.ErrNotWaiting) {
		jsonError(w, http.StatusGone, "this execution is no longer waiting")
		return
	}
	if err != nil {
		h.log.Error("resume", "execution", parts[0], "err", err)
		jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"executionId": exec.ID, "status": "resumed"})
}
