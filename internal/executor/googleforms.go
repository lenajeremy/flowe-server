package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Google Forms API v1.
//
// forms.create only accepts a title — description, settings and every question
// have to follow as a batchUpdate. create_form therefore does both in sequence
// so one node call yields a usable form.

const formsAPI = "https://forms.googleapis.com/v1"

func formsBatch(ctx context.Context, token, formID string, requests []any) (string, error) {
	if formID == "" {
		return "", fmt.Errorf("this operation needs a form ID")
	}
	return googleCall(ctx, token, http.MethodPost, formsAPI+"/forms/"+formID+":batchUpdate",
		map[string]any{"requests": requests})
}

// formsQuestion builds the question object for a createItem request. The Forms
// API models each answer style as a different field, so the type drives shape,
// not just a value.
func formsQuestion(kind, options string, required bool) (map[string]any, error) {
	q := map[string]any{"required": required}
	choices := splitCSV(options)

	switch strings.ToUpper(strings.TrimSpace(kind)) {
	case "", "TEXT":
		q["textQuestion"] = map[string]any{"paragraph": false}
	case "PARAGRAPH":
		q["textQuestion"] = map[string]any{"paragraph": true}
	case "RADIO", "CHECKBOX", "DROPDOWN":
		if len(choices) == 0 {
			return nil, fmt.Errorf("a %s question needs at least one option", strings.ToLower(kind))
		}
		opts := make([]any, 0, len(choices))
		for _, c := range choices {
			opts = append(opts, map[string]any{"value": c})
		}
		q["choiceQuestion"] = map[string]any{
			"type":    strings.ToUpper(kind),
			"options": opts,
		}
	case "SCALE":
		// Options carry the bounds when given: "1,5".
		low, high := 1, 5
		if len(choices) == 2 {
			if v, err := atoiSafe(choices[0]); err == nil {
				low = v
			}
			if v, err := atoiSafe(choices[1]); err == nil {
				high = v
			}
		}
		q["scaleQuestion"] = map[string]any{"low": low, "high": high}
	case "DATE":
		q["dateQuestion"] = map[string]any{"includeYear": true}
	case "TIME":
		q["timeQuestion"] = map[string]any{"duration": false}
	default:
		return nil, fmt.Errorf("unsupported question type %q — use TEXT, PARAGRAPH, RADIO, "+
			"CHECKBOX, DROPDOWN, SCALE, DATE or TIME", kind)
	}
	return q, nil
}

func runGoogleForms(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	form := func() string { return sub(d.FormsFormId) }
	isTrue := func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

	switch d.IntegrationOp {
	case "create_form":
		title := firstNonEmpty(sub(d.FormsTitle), "Untitled form")
		created, err := googleCall(ctx, token, http.MethodPost, formsAPI+"/forms",
			map[string]any{"info": map[string]any{"title": title, "documentTitle": title}})
		if err != nil {
			return "", err
		}
		// A description can only be set afterwards, so fold it into this op rather
		// than making the caller run update_form_info to finish the job.
		if desc := sub(d.FormsDescription); desc != "" {
			id := jsonField(created, "formId")
			if id == "" {
				return created, nil
			}
			if _, err := formsBatch(ctx, token, id, []any{
				map[string]any{"updateFormInfo": map[string]any{
					"info":       map[string]any{"description": desc},
					"updateMask": "description",
				}},
			}); err != nil {
				return "", fmt.Errorf("the form was created, but its description could not be set: %w", err)
			}
		}
		return created, nil

	case "get_form":
		if form() == "" {
			return "", fmt.Errorf("get_form needs a form ID")
		}
		raw, err := googleCall(ctx, token, http.MethodGet, formsAPI+"/forms/"+form(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "add_question":
		if sub(d.FormsQuestion) == "" {
			return "", fmt.Errorf("add_question needs the question text")
		}
		q, err := formsQuestion(sub(d.FormsQuestionType), sub(d.FormsOptions), isTrue(sub(d.FormsRequired)))
		if err != nil {
			return "", err
		}
		item := map[string]any{
			"title":        sub(d.FormsQuestion),
			"questionItem": map[string]any{"question": q},
		}
		if desc := sub(d.FormsDescription); desc != "" {
			item["description"] = desc
		}
		return formsBatch(ctx, token, form(), []any{
			map[string]any{"createItem": map[string]any{
				"item":     item,
				"location": map[string]any{"index": d.FormsIndex},
			}},
		})

	case "update_form_info":
		info, mask := map[string]any{}, []string{}
		if v := sub(d.FormsTitle); v != "" {
			info["title"] = v
			mask = append(mask, "title")
		}
		if v := sub(d.FormsDescription); v != "" {
			info["description"] = v
			mask = append(mask, "description")
		}
		if len(mask) == 0 {
			return "", fmt.Errorf("update_form_info needs a title or description")
		}
		return formsBatch(ctx, token, form(), []any{
			map[string]any{"updateFormInfo": map[string]any{
				"info": info, "updateMask": strings.Join(mask, ","),
			}},
		})

	case "set_quiz_mode":
		return formsBatch(ctx, token, form(), []any{
			map[string]any{"updateSettings": map[string]any{
				"settings": map[string]any{
					"quizSettings": map[string]any{"isQuiz": isTrue(sub(d.FormsIsQuiz))},
				},
				"updateMask": "quizSettings.isQuiz",
			}},
		})

	case "delete_item":
		if sub(d.FormsItemId) == "" && d.FormsIndex == 0 {
			return "", fmt.Errorf("delete_item needs the item's index")
		}
		return formsBatch(ctx, token, form(), []any{
			map[string]any{"deleteItem": map[string]any{
				"location": map[string]any{"index": d.FormsIndex},
			}},
		})

	case "list_responses":
		if form() == "" {
			return "", fmt.Errorf("list_responses needs a form ID")
		}
		path := fmt.Sprintf("%s/forms/%s/responses?pageSize=%d", formsAPI, form(), intOr(d.FormsLimit, 25))
		raw, err := googleCall(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_response":
		if form() == "" || sub(d.FormsResponseId) == "" {
			return "", fmt.Errorf("get_response needs a form ID and a response ID")
		}
		return googleCall(ctx, token, http.MethodGet,
			formsAPI+"/forms/"+form()+"/responses/"+sub(d.FormsResponseId), nil)

	case "set_publish_settings":
		// Whether the form accepts new responses.
		if form() == "" {
			return "", fmt.Errorf("set_publish_settings needs a form ID")
		}
		accepting := isTrue(firstNonEmpty(sub(d.FormsAccepting), "true"))
		return googleCall(ctx, token, http.MethodPost, formsAPI+"/forms/"+form()+":setPublishSettings",
			map[string]any{
				"publishSettings": map[string]any{
					"publishState": map[string]any{"isPublished": true, "isAcceptingResponses": accepting},
				},
				"updateMask": "publishState.isAcceptingResponses",
			})

	case "":
		return "", fmt.Errorf("no Google Forms operation selected")
	}
	return "", fmt.Errorf("unsupported Google Forms operation: %s", d.IntegrationOp)
}
