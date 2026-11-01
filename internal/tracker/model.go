package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EventType        = "EventWorkflow"
	DefaultTaskQueue = "event-leads"
	PageSize         = 100
)

type Event struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	EndDate     string     `json:"endDate"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	ClosesAt    time.Time  `json:"closesAt"`
	CompletesAt time.Time  `json:"completesAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
	Status      string     `json:"status"`
	Count       int        `json:"count"`
	Revision    int64      `json:"revision"`
	Form        *Form      `json:"form,omitempty"`
	QRBanner    string     `json:"qrBanner,omitempty"`
	// Slug is the link the event shares now; empty means its ID. The ID and every
	// alias keep routing to the event for as long as it is retained.
	Slug    string   `json:"slug,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	// Preview is how the event's link unfurls in Slack and other apps; nil means defaults.
	Preview *LinkPreview `json:"preview,omitempty"`
	// InternalOnly events accept responses only from signed-in employees, whose
	// name and email come from the catalog instead of the form. Once set, it stays.
	InternalOnly bool `json:"internalOnly,omitempty"`
	// Subheading sits above the event name on the attendee form. Nil means
	// DefaultSubheading; an empty string shows none.
	Subheading *string `json:"subheading,omitempty"`
}

const DefaultSubheading = "Let's stay in touch"

// EffectiveSubheading is what the attendee form shows above the event name.
func (e Event) EffectiveSubheading() string {
	if e.Subheading == nil {
		return DefaultSubheading
	}
	return *e.Subheading
}

// LinkPreview is the title and description an event's link shows when it is
// shared. Empty fields fall back to the event's name and welcome message.
type LinkPreview struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

const defaultPreviewDescription = "Stay in touch with Temporal after your event."

func (p LinkPreview) Normalize() (LinkPreview, error) {
	p.Title = strings.TrimSpace(p.Title)
	p.Description = strings.TrimSpace(p.Description)
	if utf8.RuneCountInString(p.Title) > 120 {
		return p, errors.New("link preview title must be at most 120 characters")
	}
	if utf8.RuneCountInString(p.Description) > 300 {
		return p, errors.New("link preview description must be at most 300 characters")
	}
	return p, nil
}

// EffectivePreview is what the event's link shows, with defaults filled in.
func (e Event) EffectivePreview() LinkPreview {
	var preview LinkPreview
	if e.Preview != nil {
		preview = *e.Preview
	}
	if preview.Title == "" {
		preview.Title = e.Name
	}
	if preview.Description == "" {
		preview.Description = e.Description
	}
	if preview.Description == "" {
		preview.Description = defaultPreviewDescription
	}
	return preview
}

// Link is the slug for the event's public URL and QR code.
func (e Event) Link() string {
	if e.Slug == "" {
		return e.ID
	}
	return e.Slug
}

func (e Event) At(now time.Time) Event {
	e.Status = "open"
	if e.EndedAt != nil || !now.Before(e.ClosesAt) {
		e.Status = "ended"
	}
	if !now.Before(e.CompletesAt) {
		e.Status = "archived"
	}
	return e
}

type CreateEvent struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	EndDate      string       `json:"endDate"`
	Form         *Form        `json:"form,omitempty"`
	QRBanner     string       `json:"qrBanner,omitempty"`
	Preview      *LinkPreview `json:"preview,omitempty"`
	InternalOnly bool         `json:"internalOnly,omitempty"`
	Subheading   *string      `json:"subheading,omitempty"`
}

// EventDetails are the fields an employee sets at creation and can edit later.
// A nil Subheading leaves the current one unchanged.
type EventDetails struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	EndDate     string  `json:"endDate"`
	Subheading  *string `json:"subheading,omitempty"`
}

// Normalize trims and validates the details, and returns when submissions close.
// Whether that is still in the future is left to the caller: an unchanged end
// date may already have passed.
func (d EventDetails) Normalize() (EventDetails, time.Time, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	if d.Name == "" || utf8.RuneCountInString(d.Name) > 120 {
		return d, time.Time{}, errors.New("event name must contain 1–120 characters")
	}
	if utf8.RuneCountInString(d.Description) > 1000 {
		return d, time.Time{}, errors.New("description must be at most 1,000 characters")
	}
	if d.Subheading != nil {
		subheading := strings.TrimSpace(*d.Subheading)
		if utf8.RuneCountInString(subheading) > 120 {
			return d, time.Time{}, errors.New("subheading must be at most 120 characters")
		}
		d.Subheading = &subheading
	}
	date, err := time.Parse("2006-01-02", d.EndDate)
	if err != nil {
		return d, time.Time{}, errors.New("end date must be YYYY-MM-DD")
	}
	return d, date.AddDate(0, 0, 1), nil
}

var errEndDatePassed = errors.New("end date has already passed in UTC")

func NewEvent(id, employee string, input CreateEvent, now time.Time) (Event, error) {
	details, closes, err := EventDetails{Name: input.Name, Description: input.Description, EndDate: input.EndDate, Subheading: input.Subheading}.Normalize()
	if err != nil {
		return Event{}, err
	}
	if !closes.After(now) {
		return Event{}, errEndDatePassed
	}
	input.Name, input.Description = details.Name, details.Description
	form := DefaultForm()
	if input.Form != nil {
		form = *input.Form
	}
	form, err = form.Normalize()
	if err != nil {
		return Event{}, err
	}
	if input.InternalOnly {
		if err := form.RequireIdentityFields(); err != nil {
			return Event{}, err
		}
	}
	banner, err := NormalizeBanner(input.QRBanner)
	if err != nil {
		return Event{}, err
	}
	var preview *LinkPreview
	if input.Preview != nil {
		normalized, err := input.Preview.Normalize()
		if err != nil {
			return Event{}, err
		}
		if normalized != (LinkPreview{}) {
			preview = &normalized
		}
	}
	return Event{ID: id, Name: input.Name, Description: input.Description, EndDate: input.EndDate, CreatedBy: employee, CreatedAt: now.UTC(), ClosesAt: closes, CompletesAt: closes.AddDate(0, 0, 7), Status: "open", Form: &form, QRBanner: banner, Preview: preview, InternalOnly: input.InternalOnly, Subheading: details.Subheading}, nil
}

type Submission struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Email     string   `json:"email"`
	Reason    string   `json:"reason"`
	RequestID string   `json:"requestId"`
	Answers   []Answer `json:"answers,omitempty"`
}

func (s Submission) Normalize() (Submission, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.Role = strings.TrimSpace(s.Role)
	s.Email = strings.ToLower(strings.TrimSpace(s.Email))
	s.Reason = strings.TrimSpace(s.Reason)
	if s.Name == "" || utf8.RuneCountInString(s.Name) > 120 {
		return s, errors.New("name must contain 1–120 characters")
	}
	if s.Role == "" || utf8.RuneCountInString(s.Role) > 120 {
		return s, errors.New("title / role must contain 1–120 characters")
	}
	// ParseAddress's disabled debug logger is an analyzer false positive; parsing is pure.
	address, err := mail.ParseAddress(s.Email) //workflowcheck:ignore
	if err != nil || address.Address != s.Email || len(s.Email) > 254 || !strings.Contains(s.Email, "@") {
		return s, errors.New("enter a valid email address")
	}
	if utf8.RuneCountInString(s.Reason) > 1000 {
		return s, errors.New("follow-up reason must be at most 1,000 characters")
	}
	if len(s.RequestID) < 16 || len(s.RequestID) > 80 {
		return s, errors.New("invalid submission request ID")
	}
	for _, c := range s.RequestID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return s, errors.New("invalid submission request ID")
		}
	}
	return s, nil
}

type Lead struct {
	Name             string    `json:"name"`
	Role             string    `json:"role"`
	Email            string    `json:"email"`
	Reason           string    `json:"reason"`
	FirstSubmittedAt time.Time `json:"firstSubmittedAt"`
	LastSubmittedAt  time.Time `json:"lastSubmittedAt"`
	Sequence         int64     `json:"sequence"`
	RequestID        string    `json:"requestId"`
	Answers          []Answer  `json:"answers,omitempty"`
}

// EventState is an event workflow's working copy of what the store holds.
type EventState struct {
	Event Event `json:"event"`
	// Sequence orders participant writes, so an older attempt never replaces a newer one.
	Sequence int64 `json:"sequence"`
	// Epoch fences writers: each run that takes over the event gets a new one, and
	// the store rejects writes from any other.
	Epoch int64 `json:"epoch"`
}

// Receipt records which details a submission request ID carried, so a retry is
// acknowledged without writing again and a reused ID with new details is refused.
type Receipt struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

func (s Submission) Digest() string {
	var data strings.Builder
	for _, value := range []string{s.Name, s.Role, s.Email, s.Reason} {
		data.WriteString(strconv.Itoa(len(value)))
		data.WriteByte(':')
		data.WriteString(value)
	}
	if len(s.Answers) > 0 {
		// The ordered slice gives retries a stable digest without changing legacy digests.
		encoded, _ := json.Marshal(s.Answers) //workflowcheck:ignore
		data.WriteString("answers:")
		data.Write(encoded)
	}
	sum := sha256.Sum256([]byte(data.String()))
	return hex.EncodeToString(sum[:])
}

// Existing is what a submission needs to know about the person's earlier
// response and about its own request ID, without their answers.
type Existing struct {
	Found            bool      `json:"found,omitempty"`
	RequestID        string    `json:"requestId,omitempty"`
	FirstSubmittedAt time.Time `json:"firstSubmittedAt"`
	Digest           string    `json:"digest,omitempty"`
	// Receipt is the earlier submission with this request ID, if any.
	Receipt *Receipt `json:"receipt,omitempty"`
}

func (l Lead) Digest() string {
	return Submission{Name: l.Name, Role: l.Role, Email: l.Email, Reason: l.Reason, Answers: l.Answers}.Digest()
}

// MaxSlugLength keeps "/" + slug within the 40 bytes the QR code reserves for an event path.
const MaxSlugLength = 39

// reservedSlugs are top-level paths the web server already serves.
var reservedSlugs = map[string]bool{"admin": true, "api": true, "events": true, "static": true, "healthz": true, "readyz": true}

// Slug turns an event name into a URL path segment: "KubeCon 2026" becomes
// "kubecon-2026". Attempt 0 returns the base slug; later attempts append
// "-2", "-3", and so on. It returns "" when the name has no ASCII letters or
// digits, or when the base slug is reserved and attempt is 0.
func Slug(name string, attempt int) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(c)
			dash = false
		} else {
			dash = true
		}
	}
	base := b.String()
	if base == "" {
		return ""
	}
	suffix := ""
	if attempt > 0 {
		suffix = "-" + strconv.Itoa(attempt+1)
	} else if reservedSlugs[base] {
		return ""
	}
	if len(base)+len(suffix) > MaxSlugLength {
		base = strings.TrimRight(base[:MaxSlugLength-len(suffix)], "-")
	}
	return base + suffix
}

// MaxAliases bounds how many extra slugs one event reserves.
const MaxAliases = 10

// NormalizeSlug turns an employee-chosen link into a slug with the same rules
// as event creation, so "KubeCon 2026" and "kubecon-2026" both work.
func NormalizeSlug(input string) (string, error) {
	slug := Slug(input, 0)
	if slug == "" {
		return "", errors.New("event link needs letters or digits and can't be a reserved path")
	}
	return slug, nil
}

func EventID(id string) string { return "event/" + id }
