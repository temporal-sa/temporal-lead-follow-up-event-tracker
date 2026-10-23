package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EventType         = "EventWorkflow"
	ShardType         = "ParticipantShardWorkflow"
	DefaultTaskQueue  = "event-leads"
	MaxParticipants   = 5000
	MaxShardBytes     = 1024 * 1024
	RecentRequests    = 1024
	PageSize          = 100
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
	Busy        bool       `json:"busy"`
	Shards      []string   `json:"shards"`
	Form        *Form      `json:"form,omitempty"`
	QRBanner    string     `json:"qrBanner,omitempty"`
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
	Name        string `json:"name"`
	Description string `json:"description"`
	EndDate     string `json:"endDate"`
	Form        *Form  `json:"form,omitempty"`
	QRBanner    string `json:"qrBanner,omitempty"`
}

func NewEvent(id, employee string, input CreateEvent, now time.Time) (Event, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 120 {
		return Event{}, errors.New("event name must contain 1–120 characters")
	}
	if utf8.RuneCountInString(input.Description) > 1000 {
		return Event{}, errors.New("description must be at most 1,000 characters")
	}
	date, err := time.Parse("2006-01-02", input.EndDate)
	if err != nil {
		return Event{}, errors.New("end date must be YYYY-MM-DD")
	}
	closes := date.AddDate(0, 0, 1)
	if !closes.After(now) {
		return Event{}, errors.New("end date has already passed in UTC")
	}
	form := DefaultForm()
	if input.Form != nil {
		form = *input.Form
	}
	form, err = form.Normalize()
	if err != nil {
		return Event{}, err
	}
	banner, err := NormalizeBanner(input.QRBanner)
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, Name: input.Name, Description: input.Description, EndDate: input.EndDate, CreatedBy: employee, CreatedAt: now.UTC(), ClosesAt: closes, CompletesAt: closes.AddDate(0, 0, 7), Shards: []string{}, Status: "open", Form: &form, QRBanner: banner}, nil
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

type EventState struct {
	Event    Event     `json:"event"`
	Sequence int64     `json:"sequence"`
	Receipts []Receipt `json:"receipts"`
}

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

type ShardState struct {
	EventID string `json:"eventId"`
	Leads   []Lead `json:"leads"`
	// The high-water mark makes old Activity attempts harmless after a relocation.
	LastSequence int64 `json:"lastSequence"`
}

type WriteCommand struct {
	ShardID string `json:"shardId"`
	Lead    Lead   `json:"lead"`
	Delete  bool   `json:"delete"`
}

type WriteResult struct {
	Full bool `json:"full"`
}
type LocateInput struct {
	Shards []string `json:"shards"`
	Email  string   `json:"email"`
}
// LocatedLead is what the coordinator needs about an existing participant,
// without copying their answers into its history.
type LocatedLead struct {
	ShardID          string    `json:"shardId"`
	Found            bool      `json:"found,omitempty"`
	RequestID        string    `json:"requestId,omitempty"`
	FirstSubmittedAt time.Time `json:"firstSubmittedAt"`
	Digest           string    `json:"digest,omitempty"`
	// Lead is only present in results recorded before this was slimmed down.
	// Resolved derives the same fields from it, so those histories replay unchanged.
	Lead *Lead `json:"lead,omitempty"`
}

func (l LocatedLead) Resolved() LocatedLead {
	if l.Lead != nil {
		l.Found, l.RequestID, l.FirstSubmittedAt, l.Digest = true, l.Lead.RequestID, l.Lead.FirstSubmittedAt, l.Lead.Digest()
		l.Lead = nil
	}
	return l
}

func (l Lead) Digest() string {
	return Submission{Name: l.Name, Role: l.Role, Email: l.Email, Reason: l.Reason, Answers: l.Answers}.Digest()
}
type ShardPage struct {
	Leads []Lead `json:"leads"`
	Total int    `json:"total"`
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

func EventID(id string) string           { return "event/" + id }
func ShardID(event string, n int) string { return fmt.Sprintf("event/%s/participants/%d", event, n) }

func (s *ShardState) Find(email string) *Lead {
	for i := range s.Leads {
		if s.Leads[i].Email == email {
			lead := s.Leads[i]
			return &lead
		}
	}
	return nil
}

// Apply performs no I/O and is also used to exercise the actual capacity boundary.
func (s *ShardState) Apply(c WriteCommand) (WriteResult, error) {
	if c.Lead.Sequence <= s.LastSequence {
		return WriteResult{}, nil
	}
	index := -1
	for i := range s.Leads {
		if s.Leads[i].Email == c.Lead.Email {
			index = i
			break
		}
	}
	if c.Delete {
		if index >= 0 {
			s.Leads = append(s.Leads[:index], s.Leads[index+1:]...)
		}
		s.LastSequence = c.Lead.Sequence
		return WriteResult{}, nil
	}
	if index < 0 && len(s.Leads) >= MaxParticipants {
		return WriteResult{Full: true}, nil
	}
	previous := Lead{}
	if index >= 0 {
		previous = s.Leads[index]
		s.Leads[index] = c.Lead
	} else {
		s.Leads = append(s.Leads, c.Lead)
	}
	// This concrete state contains structs/slices only; JSON map-order warnings do not apply.
	data, err := json.Marshal(s) //workflowcheck:ignore
	if err != nil {
		return WriteResult{}, err
	}
	// Leave room for the updated sequence number and Continue-As-New envelope.
	if len(data)+256 > MaxShardBytes {
		if index >= 0 {
			s.Leads[index] = previous
		} else {
			s.Leads = s.Leads[:len(s.Leads)-1]
		}
		return WriteResult{Full: true}, nil
	}
	s.LastSequence = c.Lead.Sequence
	return WriteResult{}, nil
}
