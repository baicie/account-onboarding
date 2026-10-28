package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/mailx"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func (e *Engine) stepValidate(ctx context.Context, rc *RunContext) (Outcome, error) {
	tmpl := rc.Template
	if rc.Run.Mode != tmpl.Mode {
		return Outcome{}, errs.New(errs.ClassValidation, "step.validate",
			fmt.Sprintf("template %s requires mode %s", tmpl.Name, tmpl.Mode))
	}
	for _, kind := range tmpl.RequiredKinds {
		if err := e.resolveKind(rc, kind); err != nil {
			return Outcome{}, err
		}
	}
	if needsMailbox(tmpl) && len(e.cfg.Domains) == 0 {
		return Outcome{}, errs.New(errs.ClassValidation, "step.validate", "no mailbox domain configured")
	}
	if tmpl.Mode == "real-account" && strings.TrimSpace(rc.Run.ExistingIdentityRef) == "" {
		return Outcome{}, errs.New(errs.ClassValidation, "step.validate",
			"existing_identity_ref is required for real-account runs")
	}
	out := rc.Success()
	out.Result = map[string]any{
		"provider":         rc.Selection.Mailbox.Name,
		"contract_version": rc.Selection.Mailbox.ContractVersion,
	}
	return out, nil
}

func (e *Engine) resolveKind(rc *RunContext, kind provider.Kind) error {
	var err error
	switch kind {
	case provider.KindMailbox:
		_, err = rc.MailboxProvider()
	case provider.KindIdentity:
		_, err = rc.IdentityProvider()
	case provider.KindOAuth:
		_, err = rc.OAuthProvider()
	case provider.KindEmail:
		_, err = rc.EmailManager()
	case provider.KindCredential:
		_, err = rc.CredentialVerifier()
	case provider.KindSandbox:
		_, err = rc.SandboxDriver()
	default:
		return errs.New(errs.ClassProviderBug, "step.validate", "unsupported provider kind")
	}
	return err
}

func needsMailbox(t *Template) bool {
	for _, s := range t.Steps {
		if s.Name == "prepare_mailbox" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// prepare_mailbox
// ---------------------------------------------------------------------------

func (e *Engine) stepPrepareMailbox(ctx context.Context, rc *RunContext) (Outcome, error) {
	mp, err := rc.MailboxProvider()
	if err != nil {
		return Outcome{}, err
	}
	domain := ""
	if len(e.cfg.Domains) > 0 {
		domain = e.cfg.Domains[0]
	}
	name := "onboard" + shortID(rc.Run.ID)
	providerName := mp.Descriptor().Name
	fingerprint := externalRequestFingerprint(providerName, name, domain)
	// Write-ahead gate: acquire the durable IN_FLIGHT permit before ANY remote
	// call. If this fails we must not call the provider; an ambiguous prior
	// outcome blocks retries until an admin resolves it.
	op, err := rc.beginExternalOperation(ctx, providerName, "mailbox.create", fingerprint)
	if err != nil {
		return Outcome{}, err
	}
	mb, err := mp.CreateAddress(ctx, provider.CreateAddressRequest{
		Key:          rc.Run.ID,
		Name:         name,
		Domain:       domain,
		EnablePrefix: false,
	})
	if err != nil {
		status := ExternalCompletionStatus(err)
		if cerr := rc.completeExternalOperation(ctx, op, status, string(errs.ClassOf(err))); cerr != nil {
			// Leaving IN_FLIGHT is the conservative outcome; it blocks retries.
			rc.Log.Error("external operation completion failed", "error", cerr, "status", status)
		}
		return Outcome{}, err
	}
	// Confirming the operation is part of the external-write contract. If this
	// fails, the remote resource may exist but the local gate stays IN_FLIGHT
	// (manual reconciliation), never an automatic retry.
	if err := rc.completeExternalOperation(ctx, op, storage.ExternalConfirmed, ""); err != nil {
		return Outcome{}, errs.Wrap(errs.ClassInternal, "external.operation",
			"remote resource created but local confirmation failed", err)
	}
	out := rc.Success()
	out.Resources = []storage.ResourceDraft{{
		Kind:         "mailbox",
		ExternalID:   mb.ID,
		DisplayAlias: mb.Address,
		SecretRef:    mb.SecretRef,
		Extra: map[string]any{
			"address":             mb.Address,
			"provider":            mb.Provider,
			"provider_mailbox_id": mb.ID,
		},
	}}
	out.Result = map[string]any{
		"provider":         mb.Provider,
		"mailbox_alias":    mb.Address,
		"contract_version": mp.Descriptor().ContractVersion,
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// request_verification
// ---------------------------------------------------------------------------

func (e *Engine) stepRequestVerification(ctx context.Context, rc *RunContext) (Outcome, error) {
	switch rc.Template.Name {
	case "sandbox-email-verify-oidc":
		return e.requestSandboxVerification(ctx, rc)
	case "cloudflare-mail-smoke":
		// The operator sends a real message to the address out of band; the
		// next step just waits for it.
		out := rc.Success()
		out.Result = map[string]any{"waiting_for": "smoke_email"}
		return out, nil
	case "github-existing-email":
		return e.githubAddEmail(ctx, rc)
	default:
		return Outcome{}, errs.New(errs.ClassProviderBug, "step.request_verification", "template does not support this step")
	}
}

func (e *Engine) requestSandboxVerification(ctx context.Context, rc *RunContext) (Outcome, error) {
	driver, err := rc.SandboxDriver()
	if err != nil {
		return Outcome{}, err
	}
	ip, err := rc.IdentityProvider()
	if err != nil {
		return Outcome{}, err
	}
	mb, err := rc.MailboxResource(ctx)
	if err != nil {
		return Outcome{}, err
	}
	subjectID := "onboard-" + shortID(rc.Run.ID)
	password := testPassword()
	handle, err := driver.Register(ctx, provider.SandboxRegisterRequest{
		SubjectID:   subjectID,
		Email:       mb.Address,
		DisplayName: subjectID,
		Password:    password,
	})
	if err != nil {
		return Outcome{}, err
	}
	passwordRef, err := rc.NewSecret(ctx, "sandbox_password", password)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := ip.RequestVerification(ctx, provider.RequestVerificationRequest{
		SubjectID:      handle.SubjectID,
		Address:        mb.Address,
		IdempotencyKey: rc.Run.ID + ":verify",
	}); err != nil {
		return Outcome{}, err
	}
	out := rc.Success()
	out.Resources = []storage.ResourceDraft{{
		Kind:         "identity",
		ExternalID:   handle.SubjectID,
		DisplayAlias: handle.Address,
		SecretRef:    passwordRef,
		Extra: map[string]any{
			"subject_id": handle.SubjectID,
			"address":    handle.Address,
			"provider":   handle.Provider,
		},
	}}
	out.Result = map[string]any{
		"subject_alias": handle.SubjectID,
		"waiting_for":   "verification_email",
	}
	return out, nil
}

func (e *Engine) githubAddEmail(ctx context.Context, rc *RunContext) (Outcome, error) {
	em, err := rc.EmailManager()
	if err != nil {
		return Outcome{}, err
	}
	mb, err := rc.MailboxResource(ctx)
	if err != nil {
		return Outcome{}, err
	}
	addr, err := em.AddEmail(ctx, mb.Address)
	if err != nil {
		return Outcome{}, err
	}
	out := rc.Success()
	out.ActionRequired = true
	out.Wait = e.cfg.PollDelay()
	out.ResumeState = state.WaitingUserAction
	out.Result = map[string]any{
		"resource_alias": addr.Email,
		"waiting_for":    "github_email_verification",
	}
	out.Events = []storage.EventDraft{{Type: "HUMAN_ACTION_REQUIRED", Payload: map[string]any{
		"waiting_for":  "github_email_verification",
		"reason":       "the account holder must confirm the GitHub verification email",
		"resume_state": string(state.WaitingUserAction),
	}}}
	return out, nil
}

// ---------------------------------------------------------------------------
// wait_email
// ---------------------------------------------------------------------------

func (e *Engine) stepWaitEmail(ctx context.Context, rc *RunContext) (Outcome, error) {
	mp, err := rc.MailboxProvider()
	if err != nil {
		return Outcome{}, err
	}
	mb, err := rc.MailboxResource(ctx)
	if err != nil {
		return Outcome{}, err
	}
	since := rc.Run.CreatedAt().Add(-2 * time.Minute)
	if rc.Step.StartedAtMS != nil {
		since = time.UnixMilli(*rc.Step.StartedAtMS).UTC().Add(-2 * time.Minute)
	}
	msgs, err := mp.ListMessages(ctx, mb, since)
	if err != nil {
		return Outcome{}, err
	}
	hydrated, hydrateErrs := 0, 0
	fresh := make([]provider.Message, 0, len(msgs))
	for _, msg := range msgs {
		consumed, err := rc.Store().MessageConsumed(ctx, rc.Run.ID, mb.Provider, msg.ID)
		if err != nil {
			return Outcome{}, err
		}
		if consumed {
			continue
		}
		// Provider list endpoints (Mailpit, some Cloudflare deployments) return
		// metadata only. The body is required for link/code extraction, so
		// hydrate a bounded number of candidates through GetMessage.
		if msg.Text == "" && msg.HTML == "" {
			if len(fresh) >= 50 {
				break
			}
			full, err := mp.GetMessage(ctx, mb, msg.ID)
			if err != nil {
				hydrateErrs++
				rc.Log.Warn("wait_email: message body hydration failed",
					"failure_class", string(errs.ClassOf(err)),
					"error", errs.SummaryOf(err),
					"http_status", errs.HTTPStatusOf(err))
				switch errs.ClassOf(err) {
				case errs.ClassNotFound:
					continue
				case errs.ClassValidation, errs.ClassProviderBug:
					return Outcome{}, err
				default:
					return Outcome{}, err
				}
			}
			msg = full
			hydrated++
		}
		fresh = append(fresh, msg)
	}

	if rc.Template.Name == "sandbox-email-verify-oidc" {
		matcher, err := mailx.NewMatcher(mailx.Config{
			ExpectedRecipient: mb.Address,
			SenderAllowlist:   e.cfg.SenderAllowlist,
			WindowStart:       since,
			WindowEnd:         rc.Now.Add(2 * time.Minute),
			ClockSkew:         2 * time.Minute,
			MaxMessages:       200,
		})
		if err != nil {
			return Outcome{}, errs.Wrap(errs.ClassValidation, "step.wait_email",
				"invalid mailbox matching policy", err)
		}
		result, ok := matcher.MatchNeed(fresh, mailx.NeedLink)
		rc.Log.Debug("wait_email scan", "messages", len(msgs), "fresh", len(fresh),
			"hydrated", hydrated, "hydrate_errors", hydrateErrs, "matched", ok,
			"address_alias", mb.Address)
		if ok {
			out, done, err := e.confirmSandboxEmail(ctx, rc, result.Link)
			if err != nil {
				return Outcome{}, err
			}
			if done {
				// The receipt is committed atomically with this step: a crash
				// before commit leaves the message unconsumed (crash-safe), and
				// a crash after remote verification is reconciled by the
				// CheckVerification fast path on the next attempt.
				out.MailReceipts = []storage.MailReceiptDraft{{
					Provider: mb.Provider, MessageKey: result.MessageID,
				}}
				// Optionally keep what arrived so the console can show the operator the real
				// sender, subject and link. The body is never persisted.
				if e.cfg.MailSnapshot {
					subject := ""
					for _, msg := range fresh {
						if msg.ID == result.MessageID {
							subject = msg.Subject
							break
						}
					}
					if err := rc.Store().InsertMailSnapshot(ctx, storage.MailSnapshot{
						RunID: rc.Run.ID, Step: rc.Step.Name, MailboxAlias: mb.Address,
						Sender: result.Sender, Subject: subject, Link: result.Link,
						ReceivedAtMS: result.ReceivedAt.UTC().UnixMilli(), CreatedAtMS: rc.Now.UnixMilli(),
					}); err != nil {
						rc.Log.Warn("wait_email: could not record the mail snapshot", "error", err)
					}
				}
				return out, nil
			}
		}
	} else {
		// cloudflare-mail-smoke: any accepted message (with or without a link)
		// completes the smoke. Recipient/sender/window checks stay fail-closed.
		for _, msg := range fresh {
			if !matchesRecipient(msg, mb.Address) || !senderAllowed(msg.Sender, e.cfg.SenderAllowlist) {
				continue
			}
			if msg.ReceivedAt.Before(since) || msg.ReceivedAt.After(rc.Now.Add(2*time.Minute)) {
				continue
			}
			out := rc.Success()
			out.MailReceipts = []storage.MailReceiptDraft{{Provider: mb.Provider, MessageKey: msg.ID}}
			out.Result = map[string]any{"resource_alias": mb.Address, "provider": mb.Provider}
			return out, nil
		}
	}
	if rc.Now.After(rc.Deadline) {
		return Outcome{}, errs.New(errs.ClassTimeoutUncertain, "step.wait_email", "MAIL_DELIVERY_TIMEOUT")
	}
	return WaitEmail(e.cfg.PollDelay(), "verification_email"), nil
}

// confirmSandboxEmail is crash-safe: if the identity is already verified (for
// example a previous attempt confirmed remotely but the commit was lost), it
// succeeds without touching the link again. It never consumes the message; the
// caller attaches the receipt to the successful step commit.
func (e *Engine) confirmSandboxEmail(ctx context.Context, rc *RunContext, link string) (Outcome, bool, error) {
	ident, _, err := rc.IdentityResource(ctx)
	if err != nil {
		return Outcome{}, false, err
	}
	ip, err := rc.IdentityProvider()
	if err != nil {
		return Outcome{}, false, err
	}
	if st, err := ip.CheckVerification(ctx, provider.CheckVerificationRequest{
		SubjectID: ident.SubjectID, Address: ident.Address,
	}); err != nil {
		return Outcome{}, false, err
	} else if st == provider.VerificationVerified {
		out := rc.Success()
		out.Result = map[string]any{"subject_alias": ident.SubjectID, "waiting_for": "done"}
		return out, true, nil
	}
	driver, err := rc.SandboxDriver()
	if err != nil {
		return Outcome{}, false, err
	}
	vs, err := driver.ConfirmEmail(ctx, provider.SandboxConfirmRequest{Identity: ident, VerificationURL: link})
	if err != nil {
		return Outcome{}, false, err
	}
	if vs != provider.VerificationVerified {
		return Outcome{}, false, nil
	}
	st, err := ip.CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: ident.SubjectID, Address: ident.Address})
	if err != nil {
		return Outcome{}, false, err
	}
	if st != provider.VerificationVerified {
		return Outcome{}, false, nil
	}
	out := rc.Success()
	out.Result = map[string]any{"subject_alias": ident.SubjectID, "waiting_for": "done"}
	return out, true, nil
}

// matchesRecipient and senderAllowed are the fail-closed pre-filter used by the
// mail-smoke template, where a message may not carry any link or code.
func matchesRecipient(msg provider.Message, address string) bool {
	return strings.EqualFold(strings.TrimSpace(msg.Recipient), strings.TrimSpace(address))
}

// senderAllowed parses the From header and matches either an exact address or
// the sender domain against the allowlist. Substring matching is deliberately
// not used: a display name must not be able to satisfy the allowlist.
func senderAllowed(sender string, allowlist []string) bool {
	addr := strings.ToLower(strings.TrimSpace(sender))
	if lt := strings.LastIndexByte(addr, '<'); lt >= 0 {
		if gt := strings.IndexByte(addr[lt:], '>'); gt > 0 {
			addr = addr[lt+1 : lt+gt]
		}
	}
	addr = strings.Trim(strings.TrimSpace(addr), `"'`)
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	domain := addr[at+1:]
	for _, allowed := range allowlist {
		a := strings.ToLower(strings.TrimSpace(allowed))
		a = strings.TrimPrefix(a, "@")
		if a == "" {
			continue
		}
		if strings.Contains(a, "@") {
			if addr == a {
				return true
			}
			continue
		}
		if domain == a {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// sandbox OAuth (authorization code + PKCE handled inside the sandbox driver)
// ---------------------------------------------------------------------------

func (e *Engine) stepSandboxOAuth(ctx context.Context, rc *RunContext) (Outcome, error) {
	driver, err := rc.SandboxDriver()
	if err != nil {
		return Outcome{}, err
	}
	ident, _, err := rc.IdentityResource(ctx)
	if err != nil {
		return Outcome{}, err
	}
	cred, err := driver.AuthorizeOIDC(ctx, provider.SandboxAuthorizeRequest{
		Identity:    ident,
		Scopes:      e.cfg.OIDCScopesOrDefault(),
		RedirectURI: e.cfg.OIDCRedirectURI,
	})
	if err != nil {
		return Outcome{}, err
	}
	result, err := driver.Validate(ctx, cred)
	if err != nil {
		return Outcome{}, err
	}
	switch result.State {
	case provider.CredentialValid:
		out := rc.Success()
		out.Resources = []storage.ResourceDraft{{
			Kind:       "credential_ref",
			ExternalID: cred.Ref,
			SecretRef:  cred.Ref,
			Extra: map[string]any{
				"issuer":   cred.Issuer,
				"subject":  cred.Subject,
				"audience": cred.Audience,
			},
		}}
		out.Result = map[string]any{
			"credential_ref_kind": "oidc",
			"subject_alias":       cred.Subject,
			"provider":            rc.Selection.OAuth.Name,
		}
		out.Events = []storage.EventDraft{{Type: "OAUTH_STARTED", Payload: map[string]any{
			"provider": rc.Selection.OAuth.Name,
		}}}
		return out, nil
	case provider.CredentialExpired:
		return Outcome{}, errs.New(errs.ClassAuthExpired, "step.oauth", "sandbox credential expired")
	default:
		return Outcome{}, errs.New(errs.ClassValidation, "step.oauth", "sandbox credential rejected")
	}
}

// ---------------------------------------------------------------------------
// OAuth start/poll for permission-gated existing accounts
// ---------------------------------------------------------------------------

func (e *Engine) stepOAuthStart(ctx context.Context, rc *RunContext) (Outcome, error) {
	op, err := rc.OAuthProvider()
	if err != nil {
		return Outcome{}, err
	}
	sess, err := op.Start(ctx, provider.StartAuthRequest{
		SubjectID:   rc.Run.ExistingIdentityRef,
		RedirectURI: e.cfg.OIDCRedirectURI,
		Scopes:      e.cfg.OIDCScopesOrDefault(),
	})
	if err != nil {
		return Outcome{}, err
	}
	urlRef := ""
	if sess.AuthorizationURL != "" {
		urlRef, err = rc.NewSecret(ctx, "oauth_url", sess.AuthorizationURL)
		if err != nil {
			return Outcome{}, err
		}
	}
	extra := map[string]any{
		"authorization_url_secret_ref": urlRef,
	}
	if !sess.ExpiresAt.IsZero() {
		extra["expires_at"] = sess.ExpiresAt.UTC().Format(time.RFC3339)
	}
	out := rc.Success()
	out.ActionRequired = true
	out.Wait = e.cfg.PollDelay()
	out.ResumeState = state.WaitingUserAction
	out.Resources = []storage.ResourceDraft{{
		Kind:       "oauth_session",
		ExternalID: sess.Ref,
		SecretRef:  sess.Ref,
		Extra:      extra,
	}}
	out.Result = map[string]any{
		"provider":    rc.Selection.OAuth.Name,
		"waiting_for": "oauth_authorization",
	}
	out.Events = []storage.EventDraft{{Type: "OAUTH_STARTED", Payload: map[string]any{
		"provider": rc.Selection.OAuth.Name,
	}}}
	return out, nil
}

func (e *Engine) stepWaitOAuth(ctx context.Context, rc *RunContext) (Outcome, error) {
	ref, err := rc.OAuthSessionRef(ctx)
	if err != nil {
		return Outcome{}, err
	}
	op, err := rc.OAuthProvider()
	if err != nil {
		return Outcome{}, err
	}
	st, err := op.Poll(ctx, ref)
	if err != nil {
		return Outcome{}, err
	}
	switch st.State {
	case provider.AuthAuthorized:
		if st.CredentialRef == "" {
			return Outcome{}, errs.New(errs.ClassProviderBug, "step.wait_oauth", "authorized without credential reference")
		}
		out := rc.Success()
		out.Resources = []storage.ResourceDraft{{
			Kind:       "credential_ref",
			ExternalID: st.CredentialRef,
			SecretRef:  st.CredentialRef,
			Extra:      map[string]any{"subject": st.SubjectID},
		}}
		out.Result = map[string]any{
			"credential_ref_kind": "oauth",
			"subject_alias":       st.SubjectID,
		}
		return out, nil
	case provider.AuthDenied:
		return Outcome{}, errs.New(errs.ClassCancelled, "step.wait_oauth", "authorization was denied by the account holder")
	case provider.AuthExpired:
		return Outcome{}, errs.New(errs.ClassAuthExpired, "step.wait_oauth", "authorization session expired")
	default:
		return WaitHuman(e.cfg.PollDelay(), "oauth_authorization"), nil
	}
}

// ---------------------------------------------------------------------------
// verify_credential / complete
// ---------------------------------------------------------------------------

func (e *Engine) stepVerifyCredential(ctx context.Context, rc *RunContext) (Outcome, error) {
	ref, err := rc.CredentialRef(ctx)
	if err != nil {
		return Outcome{}, err
	}
	cv, err := rc.CredentialVerifier()
	if err != nil {
		return Outcome{}, err
	}
	result, err := cv.Verify(ctx, ref)
	if err != nil {
		return Outcome{}, err
	}
	switch result.State {
	case provider.CredentialValid:
		out := rc.Success()
		out.Result = map[string]any{
			"subject_alias": result.SubjectID,
			"provider":      rc.Selection.Credential.Name,
		}
		return out, nil
	case provider.CredentialExpired:
		return Outcome{}, errs.New(errs.ClassAuthExpired, "step.verify_credential", "credential expired")
	case provider.CredentialInvalid:
		return Outcome{}, errs.New(errs.ClassValidation, "step.verify_credential", "credential rejected")
	default:
		return Outcome{}, errs.New(errs.ClassUpstreamTransient, "step.verify_credential", "credential verification inconclusive")
	}
}

func (e *Engine) stepGithubWaitVerification(ctx context.Context, rc *RunContext) (Outcome, error) {
	em, err := rc.EmailManager()
	if err != nil {
		return Outcome{}, err
	}
	mb, err := rc.MailboxResource(ctx)
	if err != nil {
		return Outcome{}, err
	}
	emails, err := em.ListEmails(ctx)
	if err != nil {
		return Outcome{}, err
	}
	for _, e := range emails {
		if !strings.EqualFold(e.Email, mb.Address) {
			continue
		}
		if !e.Verified {
			break
		}
		out := rc.Success()
		out.Result = map[string]any{"resource_alias": e.Email, "verification_status": "verified"}
		return out, nil
	}
	if rc.Now.After(rc.Deadline) {
		return Outcome{}, errs.New(errs.ClassTimeoutUncertain, "step.wait_human_verification",
			"GITHUB_VERIFICATION_TIMEOUT")
	}
	return WaitHuman(e.cfg.PollDelay(), "github_email_verification"), nil
}

func (e *Engine) stepComplete(ctx context.Context, rc *RunContext) (Outcome, error) {
	// Re-check the authoritative remote state before declaring success.
	em, err := rc.EmailManager()
	if err != nil {
		return Outcome{}, err
	}
	mb, err := rc.MailboxResource(ctx)
	if err != nil {
		return Outcome{}, err
	}
	emails, err := em.ListEmails(ctx)
	if err != nil {
		return Outcome{}, err
	}
	for _, addr := range emails {
		if strings.EqualFold(addr.Email, mb.Address) && addr.Verified {
			out := rc.Success()
			out.Result = map[string]any{"resource_alias": addr.Email, "verification_status": "verified"}
			return out, nil
		}
	}
	if rc.Now.After(rc.Deadline) {
		return Outcome{}, errs.New(errs.ClassTimeoutUncertain, "step.complete", "verification not confirmed before deadline")
	}
	return WaitHuman(e.cfg.PollDelay(), "github_email_verification"), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func testPassword() string {
	// Deterministic shape, random content: satisfies common complexity policies.
	return "Aa1!" + strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(security.RandomToken(18), "-", "x"), "_", "y"), "=")
}
