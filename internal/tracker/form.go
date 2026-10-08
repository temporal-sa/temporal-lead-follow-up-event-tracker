package tracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxFormSections = 20
	MaxFormFields   = 100
	MaxFormBytes    = 128 * 1024
	MaxAnswerBytes  = 128 * 1024
)

type Form struct {
	Sections []Section `json:"sections"`
}

type Section struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	Fields        []Field `json:"fields"`
	NextSectionID string  `json:"nextSectionId,omitempty"`
}

type Field struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Options     []string `json:"options,omitempty"`
	Rows        []string `json:"rows,omitempty"`
	Min         int      `json:"min,omitempty"`
	Max         int      `json:"max,omitempty"`
	Minimum     *float64 `json:"minimum,omitempty"`
	Maximum     *float64 `json:"maximum,omitempty"`
	MinLabel    string   `json:"minLabel,omitempty"`
	MaxLabel    string   `json:"maxLabel,omitempty"`
	Branches    []Branch `json:"branches,omitempty"`
}

type Branch struct {
	Value     string `json:"value"`
	SectionID string `json:"sectionId"`
}

// Grid answers are positional by row. Checkbox grid rows contain JSON string arrays.
type Answer struct {
	FieldID string   `json:"fieldId"`
	Values  []string `json:"values"`
}

func DefaultForm() Form {
	return Form{Sections: []Section{{ID: "details", Title: "Your details", Fields: []Field{
		{ID: "name", Label: "Name", Type: "short_text", Required: true},
		{ID: "role", Label: "Title / role", Type: "short_text", Required: true},
		{ID: "email", Label: "Email", Type: "email", Required: true},
		{ID: "reason", Label: "What would you like to follow up about?", Type: "paragraph"},
	}}}}
}

func (e Event) EffectiveForm() Form {
	if e.Form == nil {
		return DefaultForm()
	}
	return *e.Form
}

func NormalizeBanner(banner string) (string, error) {
	banner = strings.TrimSpace(banner)
	if utf8.RuneCountInString(banner) > 500 {
		return "", errors.New("QR banner must be at most 500 characters")
	}
	return banner, nil
}

func validFormID(id string) bool {
	if len(id) < 1 || len(id) > 80 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func normalizeChoices(values []string, limit int, label string) ([]string, error) {
	if len(values) < 1 || len(values) > limit {
		return nil, fmt.Errorf("%s must contain 1–%d entries", label, limit)
	}
	result := make([]string, len(values))
	seen := make(map[string]bool, len(values))
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || utf8.RuneCountInString(value) > 200 || seen[value] {
			return nil, fmt.Errorf("%s must be distinct and contain 1–200 characters", label)
		}
		seen[value] = true
		result[i] = value
	}
	return result, nil
}

func (f Form) Normalize() (Form, error) {
	if len(f.Sections) < 1 || len(f.Sections) > MaxFormSections {
		return Form{}, fmt.Errorf("form must contain 1–%d sections", MaxFormSections)
	}
	f.Sections = append([]Section(nil), f.Sections...)
	sectionIndexes := make(map[string]int, len(f.Sections))
	for i, section := range f.Sections {
		section.ID = strings.TrimSpace(section.ID)
		if !validFormID(section.ID) || section.ID == "submit" {
			return Form{}, errors.New("sections need valid IDs; submit is reserved")
		}
		if _, exists := sectionIndexes[section.ID]; exists {
			return Form{}, errors.New("section IDs must be unique")
		}
		sectionIndexes[section.ID] = i
		f.Sections[i] = section
	}
	fieldIDs := make(map[string]bool)
	emailFound, fieldCount := false, 0
	for i, section := range f.Sections {
		section.Title = strings.TrimSpace(section.Title)
		section.Description = strings.TrimSpace(section.Description)
		section.NextSectionID = strings.TrimSpace(section.NextSectionID)
		if utf8.RuneCountInString(section.Title) > 200 || utf8.RuneCountInString(section.Description) > 1000 {
			return Form{}, errors.New("section titles must be at most 200 characters and descriptions at most 1,000")
		}
		if err := validateDestination(section.NextSectionID, i, sectionIndexes); err != nil {
			return Form{}, err
		}
		section.Fields = append([]Field{}, section.Fields...)
		branchingFields := 0
		for j, field := range section.Fields {
			fieldCount++
			field.ID = strings.TrimSpace(field.ID)
			field.Label = strings.TrimSpace(field.Label)
			field.Description = strings.TrimSpace(field.Description)
			field.MinLabel = strings.TrimSpace(field.MinLabel)
			field.MaxLabel = strings.TrimSpace(field.MaxLabel)
			if !validFormID(field.ID) || fieldIDs[field.ID] {
				return Form{}, errors.New("field IDs must be valid and unique")
			}
			fieldIDs[field.ID] = true
			if field.Label == "" || utf8.RuneCountInString(field.Label) > 200 || utf8.RuneCountInString(field.Description) > 1000 {
				return Form{}, errors.New("field labels need 1–200 characters and descriptions at most 1,000")
			}
			if utf8.RuneCountInString(field.MinLabel) > 200 || utf8.RuneCountInString(field.MaxLabel) > 200 {
				return Form{}, errors.New("scale labels must be at most 200 characters")
			}
			choice, grid := false, false
			switch field.Type {
			case "short_text", "paragraph", "email", "phone", "url", "number", "date", "time":
			case "multiple_choice", "dropdown", "checkboxes":
				choice = true
			case "multiple_choice_grid", "checkbox_grid":
				choice, grid = true, true
			case "linear_scale", "rating":
				if field.Max == 0 {
					field.Max = 5
					if field.Min == 0 {
						field.Min = 1
					}
				}
				if field.Type == "rating" {
					field.Min = 1
				}
				if field.Min < 0 || field.Min > 1 || field.Max < 2 || field.Max > 10 {
					return Form{}, errors.New("scales start at 0 or 1 and end at 2–10; ratings start at 1")
				}
			default:
				return Form{}, fmt.Errorf("unsupported field type %q", field.Type)
			}
			if field.Type == "number" {
				if field.Minimum != nil && (math.IsNaN(*field.Minimum) || math.IsInf(*field.Minimum, 0)) || field.Maximum != nil && (math.IsNaN(*field.Maximum) || math.IsInf(*field.Maximum, 0)) {
					return Form{}, errors.New("number bounds must be finite")
				}
				if field.Minimum != nil && field.Maximum != nil && *field.Minimum > *field.Maximum {
					return Form{}, errors.New("number minimum cannot exceed maximum")
				}
			} else if field.Minimum != nil || field.Maximum != nil {
				return Form{}, errors.New("only number questions can define numeric bounds")
			}
			var err error
			if choice {
				field.Options, err = normalizeChoices(field.Options, 50, "choices")
				if err != nil {
					return Form{}, err
				}
			} else if len(field.Options) > 0 {
				return Form{}, errors.New("only choice questions can define choices")
			}
			if grid {
				field.Rows, err = normalizeChoices(field.Rows, 25, "grid rows")
				if err != nil {
					return Form{}, err
				}
			} else if len(field.Rows) > 0 {
				return Form{}, errors.New("only grids can define rows")
			}
			if field.ID == "email" {
				if field.Type != "email" || !field.Required || i != 0 {
					return Form{}, errors.New("the email field must be a required email question in the first section")
				}
				emailFound = true
			}
			if len(field.Branches) > 0 {
				branchingFields++
				if field.Type != "multiple_choice" && field.Type != "dropdown" {
					return Form{}, errors.New("branching requires a multiple choice or dropdown question")
				}
				seen := make(map[string]bool)
				field.Branches = append([]Branch(nil), field.Branches...)
				for k, branch := range field.Branches {
					branch.Value = strings.TrimSpace(branch.Value)
					branch.SectionID = strings.TrimSpace(branch.SectionID)
					if !contains(field.Options, branch.Value) || seen[branch.Value] || branch.SectionID == "" {
						return Form{}, errors.New("branches need a distinct existing choice and destination")
					}
					if err := validateDestination(branch.SectionID, i, sectionIndexes); err != nil {
						return Form{}, err
					}
					seen[branch.Value] = true
					field.Branches[k] = branch
				}
			}
			section.Fields[j] = field
		}
		if branchingFields > 1 {
			return Form{}, errors.New("each section can have only one branching question")
		}
		f.Sections[i] = section
	}
	if !emailFound || fieldCount > MaxFormFields {
		return Form{}, fmt.Errorf("form needs a required email field and at most %d questions", MaxFormFields)
	}
	data, err := json.Marshal(f)
	if err != nil || len(data) > MaxFormBytes {
		return Form{}, errors.New("form definition is too large")
	}
	return f, nil
}

func validateDestination(destination string, from int, indexes map[string]int) error {
	if destination == "" || destination == "submit" {
		return nil
	}
	index, found := indexes[destination]
	if !found || index <= from {
		return errors.New("section destinations must point to a later section or submit")
	}
	return nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func AnswerValues(lead Lead, fieldID string) []string {
	for _, answer := range lead.Answers {
		if answer.FieldID == fieldID {
			return answer.Values
		}
	}
	var value string
	switch fieldID {
	case "name":
		value = lead.Name
	case "role":
		value = lead.Role
	case "email":
		value = lead.Email
	case "reason":
		value = lead.Reason
	}
	if value == "" {
		return nil
	}
	return []string{value}
}

func (s Submission) NormalizeForEvent(event Event) (Submission, error) {
	if event.Form == nil {
		seen := make(map[string]bool, len(s.Answers))
		for _, answer := range s.Answers {
			if seen[answer.FieldID] {
				return s, errors.New("answers contain a duplicate field")
			}
			seen[answer.FieldID] = true
			if len(answer.Values) > 1 {
				return s, errors.New("legacy fields accept one answer")
			}
			value := ""
			if len(answer.Values) == 1 {
				value = answer.Values[0]
			}
			switch answer.FieldID {
			case "name":
				s.Name = value
			case "role":
				s.Role = value
			case "email":
				s.Email = value
			case "reason":
				s.Reason = value
			default:
				return s, errors.New("unknown field")
			}
		}
		// Nil schemas identify existing histories; preserve their exact validation and digest.
		s.Answers = nil
		return s.Normalize()
	}
	if len(s.RequestID) < 16 || len(s.RequestID) > 80 {
		return s, errors.New("invalid submission request ID")
	}
	for _, c := range s.RequestID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return s, errors.New("invalid submission request ID")
		}
	}
	data, err := json.Marshal(s.Answers) //workflowcheck:ignore
	if err != nil || len(data) > MaxAnswerBytes {
		return s, errors.New("answers are too large")
	}
	provided := make(map[string][]string, len(s.Answers))
	known := make(map[string]bool)
	for _, section := range event.Form.Sections {
		for _, field := range section.Fields {
			known[field.ID] = true
		}
	}
	for _, answer := range s.Answers {
		if _, duplicate := provided[answer.FieldID]; duplicate || !known[answer.FieldID] {
			return s, errors.New("answers contain an unknown or duplicate field")
		}
		provided[answer.FieldID] = answer.Values
	}
	if len(s.Answers) == 0 {
		for _, answer := range []Answer{{"name", []string{s.Name}}, {"role", []string{s.Role}}, {"email", []string{s.Email}}, {"reason", []string{s.Reason}}} {
			if known[answer.FieldID] {
				provided[answer.FieldID] = answer.Values
			}
		}
	}
	s.Name, s.Role, s.Email, s.Reason = "", "", "", ""
	s.Answers = nil
	for index := 0; index < len(event.Form.Sections); {
		section := event.Form.Sections[index]
		next := section.NextSectionID
		for _, field := range section.Fields {
			values, err := normalizeAnswer(field, provided[field.ID])
			if err != nil {
				return s, fmt.Errorf("%s: %w", field.Label, err)
			}
			if len(values) > 0 {
				s.Answers = append(s.Answers, Answer{FieldID: field.ID, Values: values})
				for _, branch := range field.Branches {
					if values[0] == branch.Value {
						next = branch.SectionID
						break
					}
				}
				switch field.ID {
				case "name":
					s.Name = values[0]
				case "role":
					s.Role = values[0]
				case "email":
					s.Email = values[0]
				case "reason":
					s.Reason = values[0]
				}
			}
		}
		if next == "submit" {
			break
		}
		if next == "" {
			index++
			continue
		}
		found := false
		for i := index + 1; i < len(event.Form.Sections); i++ {
			if event.Form.Sections[i].ID == next {
				index, found = i, true
				break
			}
		}
		if !found {
			return s, errors.New("invalid section destination")
		}
	}
	return s, nil
}

func normalizeAnswer(field Field, input []string) ([]string, error) {
	values := append([]string(nil), input...)
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	if field.Type == "multiple_choice_grid" || field.Type == "checkbox_grid" {
		if len(values) == 0 && !field.Required {
			return nil, nil
		}
		if len(values) != len(field.Rows) {
			return nil, errors.New("answer each grid row")
		}
		any := false
		for i, value := range values {
			if field.Type == "multiple_choice_grid" {
				if value == "" && !field.Required {
					continue
				}
				if !contains(field.Options, value) {
					return nil, fmt.Errorf("choose a valid answer for %s", field.Rows[i])
				}
				any = true
			} else {
				var selected []string
				if value != "" {
					// Decoding a concrete string slice is pure; no map iteration or custom methods.
					err := json.Unmarshal([]byte(value), &selected) //workflowcheck:ignore
					if err != nil {
						return nil, errors.New("invalid grid choices")
					}
				}
				canonical, err := normalizeSelections(field.Options, selected)
				if err != nil {
					return nil, err
				}
				if field.Required && len(canonical) == 0 {
					return nil, fmt.Errorf("choose an answer for %s", field.Rows[i])
				}
				encoded, _ := json.Marshal(canonical) //workflowcheck:ignore
				values[i] = string(encoded)
				any = any || len(canonical) > 0
			}
		}
		if !any {
			return nil, nil
		}
		return values, nil
	}
	if field.Type == "checkboxes" {
		canonical, err := normalizeSelections(field.Options, values)
		if err != nil {
			return nil, err
		}
		if field.Required && len(canonical) == 0 {
			return nil, errors.New("choose at least one answer")
		}
		if len(canonical) == 0 {
			return nil, nil
		}
		return canonical, nil
	}
	if len(values) > 1 {
		return nil, errors.New("this question accepts one answer")
	}
	if len(values) == 0 || values[0] == "" {
		if field.Required {
			return nil, errors.New("an answer is required")
		}
		return nil, nil
	}
	value := values[0]
	limit := 500
	if field.Type == "paragraph" {
		limit = 5000
	}
	if utf8.RuneCountInString(value) > limit {
		return nil, fmt.Errorf("answer must be at most %d characters", limit)
	}
	switch field.Type {
	case "email":
		value = strings.ToLower(value)
		address, err := mail.ParseAddress(value) //workflowcheck:ignore
		if err != nil || address.Address != value || len(value) > 254 || !strings.Contains(value, "@") {
			return nil, errors.New("enter a valid email address")
		}
	case "url":
		address, err := url.Parse(value)
		if err != nil || address.Host == "" || (address.Scheme != "http" && address.Scheme != "https") {
			return nil, errors.New("enter an http or https URL")
		}
	case "number":
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, errors.New("enter a number")
		}
		if field.Minimum != nil && number < *field.Minimum || field.Maximum != nil && number > *field.Maximum {
			return nil, errors.New("number is outside the allowed range")
		}
		value = strconv.FormatFloat(number, 'f', -1, 64)
	case "multiple_choice", "dropdown":
		if !contains(field.Options, value) {
			return nil, errors.New("choose a valid answer")
		}
	case "linear_scale", "rating":
		number, err := strconv.Atoi(value)
		if err != nil || number < field.Min || number > field.Max {
			return nil, errors.New("choose a value in the scale")
		}
		value = strconv.Itoa(number)
	case "date":
		if date, err := time.Parse("2006-01-02", value); err != nil || date.Format("2006-01-02") != value {
			return nil, errors.New("enter a valid date")
		}
	case "time":
		if clock, err := time.Parse("15:04", value); err != nil || clock.Format("15:04") != value {
			return nil, errors.New("enter a valid time")
		}
	}
	return []string{value}, nil
}

func normalizeSelections(options, values []string) ([]string, error) {
	selected := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !contains(options, value) || selected[value] {
			return nil, errors.New("choices must be valid and distinct")
		}
		selected[value] = true
	}
	canonical := []string{}
	for _, option := range options {
		if selected[option] {
			canonical = append(canonical, option)
		}
	}
	return canonical, nil
}
