// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lonelycode/corruption-tracker/store"

	"encoding/base64"

	"github.com/lonelycode/corruption-tracker/domain"
	"github.com/lonelycode/corruption-tracker/fraudtracker"
)

// Email-alert ingestion. Courts of NZ decision notifications and Capital
// Letter digests are LICENSED emails sent to our own mailbox — parsing our
// inbox is compliant in a way scraping JDO/NZLII never was. An email is
// treated as just another document: it flows through the same extraction,
// suppression screening, and case-match suggestion as every connector, and
// lands as an UNVERIFIED claim in the review queue. Nothing skips review.
//
// Relevance gating differs from web sources because alert emails can be
// terse ("Judgment Smith v R published [link]") with no fraud vocabulary.
// An email is ingested when ANY of:
//   - a court identifier in it matches a tracked case (the strongest signal);
//   - a tracked participant's name appears in the text;
//   - it passes the fraud vocabulary filter (rich digests).
// Everything else is skipped and counted — most alert traffic is about
// matters we don't track.
//
// Phase 1 front-end: a directory of .eml files (export/drag from any mail
// client, or a mail rule). An IMAP front-end can feed the same pipeline
// once a dedicated mailbox exists.

// MailStats summarises an ingest run.
type MailStats struct {
	Files        int
	ParseErrors  int
	Duplicates   int // message-id / content already ingested
	Irrelevant   int // failed the relevance gate
	NotFraudCase int // extractor disagreed (recorded as auto-rejected)
	ClaimsNew    int
	ManualQueued int // too thin to extract, but matched a tracked case
	Quarantined  int
	Suggestions  int
	Errors       int
}

// ParsedEmail is the parser output — exported for tests.
type ParsedEmail struct {
	MessageID string
	From      string
	Subject   string
	Date      *time.Time
	Text      string   // best-effort plain text of the body
	Links     []string // http(s) links found in the body
}

var mailLinkRe = regexp.MustCompile(`https?://[^\s<>"')\]]+`)

// ParseEmailFile reads one RFC-5322 message (.eml).
func ParseEmailFile(path string) (*ParsedEmail, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseEmail(f)
}

// ParseEmail parses a message from a reader: headers, then a best-effort
// text body (text/plain preferred, text/html stripped via goquery),
// handling quoted-printable/base64 transfer encodings and one level of
// multipart nesting (alert emails are simple).
func ParseEmail(r io.Reader) (*ParsedEmail, error) {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	out := &ParsedEmail{
		MessageID: strings.Trim(msg.Header.Get("Message-Id"), "<> "),
		From:      msg.Header.Get("From"),
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err == nil {
		out.Subject = dec
	} else {
		out.Subject = msg.Header.Get("Subject")
	}
	if t, err := mail.ParseDate(msg.Header.Get("Date")); err == nil {
		tt := t.UTC()
		out.Date = &tt
	}

	plain, html := extractBodies(msg.Header.Get("Content-Type"),
		msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
	text := strings.TrimSpace(plain)
	if text == "" && html != "" {
		text = HTMLToText(html)
	}
	out.Text = text
	seen := map[string]bool{}
	for _, src := range []string{plain, html} {
		for _, l := range mailLinkRe.FindAllString(src, -1) {
			l = strings.TrimRight(l, ".,;")
			if !seen[l] {
				seen[l] = true
				out.Links = append(out.Links, l)
			}
		}
	}
	return out, nil
}

// extractBodies walks the MIME structure collecting the first text/plain
// and text/html parts (depth-limited; alert emails are shallow).
func extractBodies(contentType, encoding string, body io.Reader, depth int) (plain, html string) {
	if depth > 3 {
		return "", ""
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			p, h := extractBodies(part.Header.Get("Content-Type"),
				part.Header.Get("Content-Transfer-Encoding"), part, depth+1)
			if plain == "" {
				plain = p
			}
			if html == "" {
				html = h
			}
		}
		return plain, html
	}

	var rd io.Reader = body
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		rd = quotedprintable.NewReader(body)
	case "base64":
		rd = base64.NewDecoder(base64.StdEncoding, body)
	}
	b, err := io.ReadAll(io.LimitReader(rd, 2<<20))
	if err != nil && len(b) == 0 {
		return "", ""
	}
	switch {
	case strings.HasPrefix(mediaType, "text/html"):
		return "", string(b)
	default:
		return string(b), ""
	}
}

// IngestMailDir processes every .eml file in dir. Idempotent: dedupe is
// keyed on the message-id (falling back to a content hash), so re-running
// over the same directory is a no-op. Processed files are moved to
// archiveDir when set.
func (d *Deps) IngestMailDir(ctx context.Context, dir, archiveDir string) (MailStats, error) {
	var st MailStats
	entries, err := os.ReadDir(dir)
	if err != nil {
		return st, err
	}
	// Tracked-case matchers for the relevance gate, loaded once per run.
	gate, err := d.loadMailGate(ctx)
	if err != nil {
		return st, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".eml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	st.Files = len(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		em, err := ParseEmailFile(path)
		if err != nil {
			st.ParseErrors++
			d.Log.Warn("mail parse failed", "file", name, "err", err)
			continue
		}
		processed, err := d.ingestEmail(ctx, gate, em, &st)
		if err != nil {
			st.Errors++
			d.Log.Warn("mail ingest failed", "file", name, "err", err)
			continue
		}
		if processed && archiveDir != "" && !d.DryRun {
			if err := os.Rename(path, filepath.Join(archiveDir, name)); err != nil {
				d.Log.Warn("mail archive move failed", "file", name, "err", err)
			}
		}
	}
	return st, nil
}

// mailGate holds the tracked-case matchers for terse-alert relevance.
type mailGate struct {
	courtIDs map[string]bool
	names    []string // lowercased tracked participant names, len >= 8
}

func (d *Deps) loadMailGate(ctx context.Context) (*mailGate, error) {
	g := &mailGate{courtIDs: map[string]bool{}}
	rows, err := d.DB.Pool.Query(ctx, `SELECT unnest(court_identifiers) FROM fraud_cases`)
	if err != nil {
		return nil, fmt.Errorf("load court ids: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		g.courtIDs[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = d.DB.Pool.Query(ctx, `SELECT DISTINCT lower(full_name) FROM fraud_persons`)
	if err != nil {
		return nil, fmt.Errorf("load names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		// Short names substring-match too eagerly; tracked full names are
		// long enough for this to be a precise terse-alert signal.
		if len(n) >= 8 {
			g.names = append(g.names, n)
		}
	}
	return g, rows.Err()
}

// relevant applies the gate; it reports why (for the claim headline/log)
// and the matched participant name, if that was the trigger, so terse
// alerts can be pre-suggested onto their case by name as well as citation.
func (g *mailGate) relevant(text string) (ok bool, why, matchedName string) {
	for _, id := range fraudtracker.DetectCourtIdentifiers(text) {
		if g.courtIDs[id] {
			return true, "tracked citation " + id, ""
		}
	}
	low := strings.ToLower(text)
	for _, n := range g.names {
		if strings.Contains(low, n) {
			return true, "tracked participant", n
		}
	}
	if HasFraudVocabulary(text) {
		return true, "fraud vocabulary", ""
	}
	return false, "", ""
}

// minExtractableChars: below this the email is a bare notification; we
// queue it for manual review (with its link) instead of burning an LLM
// call that has nothing to extract.
const minExtractableChars = 400

func (d *Deps) ingestEmail(ctx context.Context, gate *mailGate, em *ParsedEmail, st *MailStats) (bool, error) {
	full := strings.TrimSpace(em.Subject + "\n\n" + em.Text)
	if full == "" {
		st.ParseErrors++
		return false, nil
	}

	key := "email:" + em.MessageID
	if em.MessageID == "" {
		sum := sha256.Sum256([]byte(full))
		key = "email:" + hex.EncodeToString(sum[:16])
	}
	exists, err := d.claimKeyExists(ctx, key)
	if err != nil {
		return false, err
	}
	if exists {
		st.Duplicates++
		return true, nil
	}

	ok, why, matchedName := gate.relevant(full)
	if !ok {
		st.Irrelevant++
		return true, nil // archived: irrelevant today stays irrelevant
	}

	sum := sha256.Sum256([]byte(full))
	hash := "email:" + hex.EncodeToString(sum[:])
	publisher := em.From
	if a, err := mail.ParseAddress(em.From); err == nil {
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			publisher = a.Address[i+1:]
		}
	}
	var link string
	if len(em.Links) > 0 {
		link = em.Links[0]
	}
	claim := domain.FraudClaim{
		Headline:        strings.TrimSpace(em.Subject),
		SourceType:      domain.FraudSourceJudgment, // court-alert mail; reviewer can reclass
		SourceURL:       link,
		SourcePublisher: publisher,
		LicenceBasis:    "licensed-email-alert (subscribed feed; our own mailbox)",
		PublishedDate:   em.Date,
		ContentHash:     hash,
		DedupeKey:       &key,
	}
	if claim.Headline == "" {
		claim.Headline = "[email alert] " + why
	}

	var personNames, orgNames, ids []string
	if d.LLM != nil && len(full) >= minExtractableChars {
		ex, raw, err := runExtraction(ctx, d.LLM, full)
		if err != nil {
			claim.NeedsManualExtraction = true
			claim.Excerpt = firstChars(full, 800)
		} else if !ex.IsFraudCase {
			// Recorded as auto-rejected: the dedupe memory that stops the
			// same digest being re-extracted on every run.
			st.NotFraudCase++
			if d.DryRun {
				return true, nil
			}
			claim.Payload = raw
			conf := ex.Confidence
			claim.ConfidenceScore = &conf
			claim.Excerpt = firstChars(full, 400)
			rejected, wasNew, err := d.DB.CreateFraudClaim(ctx, claim, nil, "fraudmail")
			if err != nil || !wasNew {
				return err == nil, err
			}
			_, err = d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
				ClaimID: rejected.ID, To: domain.ReviewRejected,
				ReviewerLabel: "fraudmail",
				Note:          "auto-rejected: extractor classified as not an enforcement matter",
			})
			return err == nil, err
		} else {
			claim.Payload = raw
			conf := ex.Confidence
			claim.ConfidenceScore = &conf
			claim.Excerpt = ex.ExactExcerpt
			suppressed, note := suppressionScreen(ex, full)
			claim.SuppressionFlag = suppressed
			if suppressed {
				claim.Headline += " [suppression: " + firstChars(note, 60) + "]"
			}
			personNames = namedPersons(ex)
			for _, o := range ex.Organisations {
				if strings.TrimSpace(o.Name) != "" {
					orgNames = append(orgNames, o.Name)
				}
			}
			ids = courtIDs(ex, full)
		}
	} else {
		// Terse notification that matched a tracked case: queue it with its
		// link for the reviewer — the automated version of "Record finding".
		claim.NeedsManualExtraction = true
		claim.Excerpt = firstChars(full, 800)
		claim.Headline += " — " + why
		ids = fraudtracker.DetectCourtIdentifiers(full)
		if matchedName != "" {
			personNames = append(personNames, matchedName)
		}
	}

	if d.DryRun {
		d.Log.Info("dry-run: would create mail claim", "subject", em.Subject, "why", why,
			"manual", claim.NeedsManualExtraction)
		st.ClaimsNew++
		return true, nil
	}

	created, wasNew, err := d.DB.CreateFraudClaim(ctx, claim, nil, "fraudmail")
	if err != nil {
		return false, err
	}
	if !wasNew {
		st.Duplicates++
		return true, nil
	}
	st.ClaimsNew++
	if claim.NeedsManualExtraction {
		st.ManualQueued++
	}
	if claim.SuppressionFlag {
		if _, err := d.DB.TransitionFraudClaim(ctx, fraudtracker.TransitionInput{
			ClaimID: created.ID, To: domain.ReviewQuarantined,
			ReviewerLabel: "fraudmail",
			Note:          "auto-quarantined: suppression signal in alert email",
		}); err != nil {
			d.Log.Warn("mail quarantine failed", "claim", created.ID, "err", err)
		} else {
			st.Quarantined++
		}
	}
	n, err := d.DB.SuggestFraudCaseMatches(ctx, store.FraudMatchInput{
		ClaimID: created.ID, PersonNames: personNames, OrgNames: orgNames, CourtIDs: ids,
	})
	if err != nil {
		d.Log.Warn("mail match suggestion failed", "claim", created.ID, "err", err)
	}
	st.Suggestions += n
	return true, nil
}
