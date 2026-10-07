package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"go.temporal.io/api/serviceerror"
)

type leadCursor struct {
	EventID  string `json:"event"`
	Revision int64  `json:"revision"`
	Shard    int    `json:"shard"`
	Offset   int    `json:"offset"`
}

func decodeLeadCursor(value string, event tracker.Event) (leadCursor, error) {
	cursor := leadCursor{EventID: event.ID, Revision: event.Revision}
	if value == "" {
		return cursor, nil
	}
	if len(value) > 2048 {
		return cursor, errInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, errInvalidCursor
	}
	if json.Unmarshal(data, &cursor) != nil || cursor.EventID != event.ID || cursor.Shard < 0 || cursor.Shard >= len(event.Shards) || cursor.Offset < 0 || cursor.Offset > tracker.MaxParticipants {
		return cursor, errInvalidCursor
	}
	if cursor.Revision != event.Revision {
		return cursor, errChanged
	}
	return cursor, nil
}

func encodeLeadCursor(cursor leadCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func (s *server) page(ctx context.Context, shard string, offset, limit int) (tracker.ShardPage, error) {
	page, err := s.gateway.Page(ctx, shard, offset, limit)
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return page, errExpired
	}
	if err == nil && (page.Total < offset || page.Total > tracker.MaxParticipants || len(page.Leads) > limit || len(page.Leads) > page.Total-offset || (len(page.Leads) == 0 && offset < page.Total)) {
		return page, errChanged
	}
	return page, err
}

func (s *server) participants(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	if event.Busy {
		handleError(w, errChanged)
		return
	}
	cursor, err := decodeLeadCursor(r.URL.Query().Get("cursor"), event)
	if err != nil {
		handleError(w, err)
		return
	}
	leads := make([]tracker.Lead, 0, tracker.PageSize)
	for cursor.Shard < len(event.Shards) && len(leads) < tracker.PageSize {
		page, err := s.page(r.Context(), event.Shards[cursor.Shard], cursor.Offset, tracker.PageSize-len(leads))
		if err != nil {
			handleError(w, err)
			return
		}
		leads = append(leads, page.Leads...)
		cursor.Offset += len(page.Leads)
		if cursor.Offset == page.Total {
			cursor.Shard++
			cursor.Offset = 0
		}
	}
	confirmed, err := s.gateway.Event(r.Context(), event.ID)
	if err != nil {
		handleError(w, err)
		return
	}
	if confirmed.Busy || confirmed.Revision != event.Revision || confirmed.Count != event.Count {
		handleError(w, errChanged)
		return
	}
	next := ""
	if cursor.Shard < len(event.Shards) {
		next = encodeLeadCursor(cursor)
	}
	writeJSON(w, http.StatusOK, struct {
		Leads      []tracker.Lead `json:"leads"`
		NextCursor string         `json:"nextCursor"`
	}{leads, next})
}

func (s *server) export(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var data []byte
	for attempt := 0; attempt < 3; attempt++ {
		data, err = s.csvSnapshot(r.Context(), id)
		if !errors.Is(err, errChanged) {
			break
		}
	}
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="event-%s-leads.csv"`, id))
	_, _ = w.Write(data)
}

func (s *server) csvSnapshot(ctx context.Context, id string) ([]byte, error) {
	event, err := s.gateway.Event(ctx, id)
	if err != nil {
		return nil, err
	}
	if event.Busy {
		return nil, errChanged
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write([]string{"event_id", "event_name", "event_end_date_utc", "name", "title_role", "email", "follow_up_reason", "first_submitted_at_utc", "last_submitted_at_utc"}); err != nil {
		return nil, err
	}
	count := 0
	emails := make(map[string]struct{}, event.Count)
	for _, shard := range event.Shards {
		for offset := 0; ; {
			page, err := s.page(ctx, shard, offset, tracker.PageSize)
			if err != nil {
				return nil, err
			}
			for _, lead := range page.Leads {
				if _, exists := emails[lead.Email]; exists {
					return nil, errChanged
				}
				emails[lead.Email] = struct{}{}
				row := []string{event.ID, event.Name, event.EndDate, lead.Name, lead.Role, lead.Email, lead.Reason, lead.FirstSubmittedAt.UTC().Format(time.RFC3339Nano), lead.LastSubmittedAt.UTC().Format(time.RFC3339Nano)}
				for i := range row {
					row[i] = safeCSVCell(row[i])
				}
				if err := writer.Write(row); err != nil {
					return nil, err
				}
				count++
			}
			offset += len(page.Leads)
			if offset == page.Total {
				break
			}
		}
	}
	confirmed, err := s.gateway.Event(ctx, id)
	if err != nil {
		return nil, err
	}
	if confirmed.Busy || confirmed.Revision != event.Revision || confirmed.Count != event.Count || count != event.Count {
		return nil, errChanged
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// CSV readers may evaluate formulas even after leading whitespace.
func safeCSVCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") {
		return "'" + value
	}
	return value
}
