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
)

// A participant cursor carries the store's position and the event revision the
// first page was read at, so pages from different versions are never mixed.
type leadCursor struct {
	EventID  string `json:"event"`
	Revision int64  `json:"revision"`
	Key      string `json:"key"`
}

func decodeLeadCursor(value string, event tracker.Event) (leadCursor, error) {
	cursor := leadCursor{EventID: event.ID, Revision: event.Revision}
	if value == "" {
		return cursor, nil
	}
	if len(value) > 8192 {
		return cursor, errInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, errInvalidCursor
	}
	if json.Unmarshal(data, &cursor) != nil || cursor.EventID != event.ID || cursor.Key == "" {
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

// unchanged confirms that the event has the revision and count it had before a
// read, so the participants read in between belong to that version.
func (s *server) unchanged(ctx context.Context, event tracker.Event) error {
	confirmed, err := s.visibleEvent(ctx, event.ID)
	if err != nil {
		return err
	}
	if confirmed.Revision != event.Revision || confirmed.Count != event.Count {
		return errChanged
	}
	return nil
}

func (s *server) participants(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	cursor, err := decodeLeadCursor(r.URL.Query().Get("cursor"), event)
	if err != nil {
		handleError(w, err)
		return
	}
	leads, next, err := s.gateway.Participants(r.Context(), event.ID, cursor.Key, tracker.PageSize)
	if err != nil {
		handleError(w, err)
		return
	}
	if err := s.unchanged(r.Context(), event); err != nil {
		handleError(w, err)
		return
	}
	nextCursor := ""
	if next != "" {
		cursor.Key = next
		nextCursor = encodeLeadCursor(cursor)
	}
	writeJSON(w, http.StatusOK, struct {
		Leads      []tracker.Lead `json:"leads"`
		NextCursor string         `json:"nextCursor"`
	}{leads, nextCursor})
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
	event, err := s.visibleEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	columns := formCSVColumns(event)
	header := []string{"event_id", "event_name", "event_end_date_utc"}
	if event.Form == nil {
		header = append(header, "name", "title_role", "email", "follow_up_reason")
	} else {
		for _, column := range columns {
			header = append(header, safeCSVCell(column.label))
		}
	}
	header = append(header, "first_submitted_at_utc", "last_submitted_at_utc")
	if err := writer.Write(header); err != nil {
		return nil, err
	}
	count := 0
	for cursor := ""; ; {
		leads, next, err := s.gateway.Participants(ctx, event.ID, cursor, tracker.PageSize)
		if err != nil {
			return nil, err
		}
		for _, lead := range leads {
			row := []string{event.ID, event.Name, event.EndDate}
			if event.Form == nil {
				row = append(row, lead.Name, lead.Role, lead.Email, lead.Reason)
			} else {
				for _, column := range columns {
					row = append(row, column.value(lead))
				}
			}
			row = append(row, lead.FirstSubmittedAt.UTC().Format(time.RFC3339Nano), lead.LastSubmittedAt.UTC().Format(time.RFC3339Nano))
			for i := range row {
				row[i] = safeCSVCell(row[i])
			}
			if err := writer.Write(row); err != nil {
				return nil, err
			}
			count++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if err := s.unchanged(ctx, event); err != nil {
		return nil, err
	}
	if count != event.Count {
		return nil, errChanged
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type formCSVColumn struct {
	label      string
	fieldID    string
	gridRow    int // -1 for a field with one column; otherwise its row position.
	checkboxes bool
}

func formCSVColumns(event tracker.Event) []formCSVColumn {
	if event.Form == nil {
		return nil
	}
	var columns []formCSVColumn
	for _, section := range event.Form.Sections {
		for _, field := range section.Fields {
			if field.Type == "multiple_choice_grid" || field.Type == "checkbox_grid" {
				for i, row := range field.Rows {
					columns = append(columns, formCSVColumn{
						label:   fmt.Sprintf("%s: %s [%s.row%d]", field.Label, row, field.ID, i+1),
						fieldID: field.ID, gridRow: i,
					})
				}
				continue
			}
			columns = append(columns, formCSVColumn{label: field.Label + " [" + field.ID + "]", fieldID: field.ID, gridRow: -1, checkboxes: field.Type == "checkboxes"})
		}
	}
	return columns
}

func (column formCSVColumn) value(lead tracker.Lead) string {
	values := tracker.AnswerValues(lead, column.fieldID)
	if column.gridRow >= 0 {
		if column.gridRow >= len(values) || values[column.gridRow] == "[]" {
			return ""
		}
		return values[column.gridRow]
	}
	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 && !column.checkboxes {
		return values[0]
	}
	// JSON keeps multi-select values unambiguous when options contain separators.
	data, _ := json.Marshal(values)
	return string(data)
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
