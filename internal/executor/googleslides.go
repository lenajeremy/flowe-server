package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Google Slides API v1. Almost every mutation goes through one batchUpdate
// endpoint that takes a list of typed requests, so the ops below are mostly
// about assembling the right request objects.
//
// The awkward part of the API is that new slides come back with anonymous
// placeholder shapes: to put text on a slide you need the objectId of its title
// and body placeholders, which you only learn after creating it. createSlide
// accepts placeholderIdMappings to name them up front, which lets add_slide
// create the slide and fill both placeholders in a single call.

const slidesAPI = "https://slides.googleapis.com/v1"

func slidesBatch(ctx context.Context, token, presentationID string, requests []any) (string, error) {
	if presentationID == "" {
		return "", fmt.Errorf("this operation needs a presentation ID")
	}
	return googleCall(ctx, token, http.MethodPost,
		slidesAPI+"/presentations/"+presentationID+":batchUpdate",
		map[string]any{"requests": requests})
}

func runGoogleSlides(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	deck := func() string { return sub(d.SlidesPresentationId) }

	switch d.IntegrationOp {
	case "create_presentation":
		return googleCall(ctx, token, http.MethodPost, slidesAPI+"/presentations",
			map[string]any{"title": firstNonEmpty(sub(d.SlidesTitle), "Untitled presentation")})

	case "get_presentation":
		if deck() == "" {
			return "", fmt.Errorf("get_presentation needs a presentation ID")
		}
		raw, err := googleCall(ctx, token, http.MethodGet, slidesAPI+"/presentations/"+deck(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "list_slides":
		// The full presentation is enormous — reduce it to the outline a workflow
		// can act on: slide ids in order, plus whatever text each one holds.
		if deck() == "" {
			return "", fmt.Errorf("list_slides needs a presentation ID")
		}
		raw, err := googleCall(ctx, token, http.MethodGet, slidesAPI+"/presentations/"+deck(), nil)
		if err != nil {
			return "", err
		}
		return slidesOutline(raw)

	case "add_slide":
		layout := firstNonEmpty(strings.ToUpper(sub(d.SlidesLayout)), "TITLE_AND_BODY")
		slideID := fmt.Sprintf("slide_%d", clockNow().UnixNano())
		titleID, bodyID := slideID+"_title", slideID+"_body"

		create := map[string]any{
			"objectId":              slideID,
			"slideLayoutReference":  map[string]string{"predefinedLayout": layout},
			"placeholderIdMappings": []any{},
		}
		if i := d.SlidesIndex; i > 0 {
			create["insertionIndex"] = i
		}
		// Only claim the placeholders the chosen layout actually has, or Slides
		// rejects the whole batch.
		mappings := []any{}
		wantTitle := layout != "BLANK"
		wantBody := layout == "TITLE_AND_BODY"
		if wantTitle {
			mappings = append(mappings, map[string]any{
				"layoutPlaceholder": map[string]any{"type": "TITLE", "index": 0},
				"objectId":          titleID,
			})
		}
		if wantBody {
			mappings = append(mappings, map[string]any{
				"layoutPlaceholder": map[string]any{"type": "BODY", "index": 0},
				"objectId":          bodyID,
			})
		}
		create["placeholderIdMappings"] = mappings

		requests := []any{map[string]any{"createSlide": create}}
		if h := sub(d.SlidesHeading); h != "" && wantTitle {
			requests = append(requests, map[string]any{
				"insertText": map[string]any{"objectId": titleID, "text": h},
			})
		}
		if b := sub(d.SlidesBody); b != "" && wantBody {
			requests = append(requests, map[string]any{
				"insertText": map[string]any{"objectId": bodyID, "text": b},
			})
		}
		if _, err := slidesBatch(ctx, token, deck(), requests); err != nil {
			return "", err
		}
		// Speaker notes live on the slide's notesPage, whose shape id only exists
		// once the slide does — so they need a second pass rather than an error.
		if n := sub(d.SlidesNotes); n != "" {
			shape, err := slidesNotesShape(ctx, token, deck(), slideID)
			if err != nil {
				return "", fmt.Errorf("slide %s was created, but its speaker notes could not be set: %w", slideID, err)
			}
			if _, err := slidesBatch(ctx, token, deck(), []any{
				map[string]any{"insertText": map[string]any{"objectId": shape, "text": n}},
			}); err != nil {
				return "", fmt.Errorf("slide %s was created, but its speaker notes could not be set: %w", slideID, err)
			}
		}
		return fmt.Sprintf(`{"ok":true,"slideId":%q,"layout":%q}`, slideID, layout), nil

	case "duplicate_slide":
		if sub(d.SlidesSlideId) == "" {
			return "", fmt.Errorf("duplicate_slide needs a slide ID")
		}
		return slidesBatch(ctx, token, deck(), []any{
			map[string]any{"duplicateObject": map[string]any{"objectId": sub(d.SlidesSlideId)}},
		})

	case "delete_slide":
		if sub(d.SlidesSlideId) == "" {
			return "", fmt.Errorf("delete_slide needs a slide ID")
		}
		return slidesBatch(ctx, token, deck(), []any{
			map[string]any{"deleteObject": map[string]any{"objectId": sub(d.SlidesSlideId)}},
		})

	case "delete_object":
		if sub(d.SlidesObjectId) == "" {
			return "", fmt.Errorf("delete_object needs an object ID")
		}
		return slidesBatch(ctx, token, deck(), []any{
			map[string]any{"deleteObject": map[string]any{"objectId": sub(d.SlidesObjectId)}},
		})

	case "replace_all_text":
		if sub(d.SlidesFind) == "" {
			return "", fmt.Errorf("replace_all_text needs the text to find")
		}
		return slidesBatch(ctx, token, deck(), []any{
			map[string]any{"replaceAllText": map[string]any{
				"containsText": map[string]any{"text": sub(d.SlidesFind), "matchCase": true},
				"replaceText":  sub(d.SlidesReplace),
			}},
		})

	case "add_text_box":
		if sub(d.SlidesSlideId) == "" {
			return "", fmt.Errorf("add_text_box needs a slide ID")
		}
		boxID := fmt.Sprintf("box_%d", clockNow().UnixNano())
		return slidesBatch(ctx, token, deck(), []any{
			map[string]any{"createShape": map[string]any{
				"objectId":  boxID,
				"shapeType": "TEXT_BOX",
				"elementProperties": map[string]any{
					"pageObjectId": sub(d.SlidesSlideId),
					"size": map[string]any{
						"width":  map[string]any{"magnitude": 3000000, "unit": "EMU"},
						"height": map[string]any{"magnitude": 1000000, "unit": "EMU"},
					},
					"transform": map[string]any{
						"scaleX": 1, "scaleY": 1, "translateX": 600000, "translateY": 1200000, "unit": "EMU",
					},
				},
			}},
			map[string]any{"insertText": map[string]any{"objectId": boxID, "text": sub(d.SlidesBody)}},
		})

	case "add_image":
		if sub(d.SlidesSlideId) == "" || sub(d.SlidesImageUrl) == "" {
			return "", fmt.Errorf("add_image needs a slide ID and an image URL")
		}
		// Slides fetches the image itself, so the URL has to be publicly
		// reachable — a signed or private link fails inside Google, not here.
		return slidesBatch(ctx, token, deck(), []any{
			map[string]any{"createImage": map[string]any{
				"url": sub(d.SlidesImageUrl),
				"elementProperties": map[string]any{
					"pageObjectId": sub(d.SlidesSlideId),
					"size": map[string]any{
						"width":  map[string]any{"magnitude": 3000000, "unit": "EMU"},
						"height": map[string]any{"magnitude": 2000000, "unit": "EMU"},
					},
					"transform": map[string]any{
						"scaleX": 1, "scaleY": 1, "translateX": 600000, "translateY": 1200000, "unit": "EMU",
					},
				},
			}},
		})

	case "update_speaker_notes":
		// The notes text lives in a shape on the slide's notesPage, whose id has
		// to be read from the slide first.
		if sub(d.SlidesSlideId) == "" {
			return "", fmt.Errorf("update_speaker_notes needs a slide ID")
		}
		notesShape, err := slidesNotesShape(ctx, token, deck(), sub(d.SlidesSlideId))
		if err != nil {
			return "", err
		}
		requests := []any{
			// Clearing first makes the op idempotent instead of appending on re-run.
			map[string]any{"deleteText": map[string]any{
				"objectId": notesShape, "textRange": map[string]any{"type": "ALL"},
			}},
			map[string]any{"insertText": map[string]any{
				"objectId": notesShape, "text": sub(d.SlidesNotes),
			}},
		}
		if _, err := slidesBatch(ctx, token, deck(), requests); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"slideId":%q,"notesUpdated":true}`, sub(d.SlidesSlideId)), nil

	case "get_thumbnail":
		if sub(d.SlidesSlideId) == "" {
			return "", fmt.Errorf("get_thumbnail needs a slide ID")
		}
		return googleCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/presentations/%s/pages/%s/thumbnail", slidesAPI, deck(), sub(d.SlidesSlideId)), nil)

	case "create_from_template":
		return slidesFromTemplate(ctx, token, d, sub)

	case "":
		return "", fmt.Errorf("no Google Slides operation selected")
	}
	return "", fmt.Errorf("unsupported Google Slides operation: %s", d.IntegrationOp)
}

// slidesOutline reduces a presentation to slide ids plus their text.
func slidesOutline(raw string) (string, error) {
	var deck struct {
		Title  string `json:"title"`
		Slides []struct {
			ObjectID     string `json:"objectId"`
			PageElements []struct {
				Shape struct {
					Text struct {
						TextElements []struct {
							TextRun struct {
								Content string `json:"content"`
							} `json:"textRun"`
						} `json:"textElements"`
					} `json:"text"`
				} `json:"shape"`
			} `json:"pageElements"`
		} `json:"slides"`
	}
	if err := json.Unmarshal([]byte(raw), &deck); err != nil {
		return "", fmt.Errorf("could not read the presentation")
	}
	type slide struct {
		Index   int    `json:"index"`
		SlideID string `json:"slideId"`
		Text    string `json:"text"`
	}
	out := make([]slide, 0, len(deck.Slides))
	for i, s := range deck.Slides {
		var b strings.Builder
		for _, el := range s.PageElements {
			for _, te := range el.Shape.Text.TextElements {
				b.WriteString(te.TextRun.Content)
			}
		}
		out = append(out, slide{
			Index:   i + 1,
			SlideID: s.ObjectID,
			Text:    strings.TrimSpace(strings.ReplaceAll(b.String(), "\n", " ")),
		})
	}
	payload, _ := json.Marshal(map[string]any{"title": deck.Title, "slideCount": len(out), "slides": out})
	return truncateStr(string(payload), 8000), nil
}

// slidesNotesShape finds the shape that holds a slide's speaker notes.
func slidesNotesShape(ctx context.Context, token, presentationID, slideID string) (string, error) {
	raw, err := googleCall(ctx, token, http.MethodGet,
		slidesAPI+"/presentations/"+presentationID+"/pages/"+slideID, nil)
	if err != nil {
		return "", err
	}
	var page struct {
		SlideProperties struct {
			NotesPage struct {
				NotesProperties struct {
					SpeakerNotesObjectID string `json:"speakerNotesObjectId"`
				} `json:"notesProperties"`
			} `json:"notesPage"`
		} `json:"slideProperties"`
	}
	if json.Unmarshal([]byte(raw), &page) != nil {
		return "", fmt.Errorf("could not read slide %s", slideID)
	}
	id := page.SlideProperties.NotesPage.NotesProperties.SpeakerNotesObjectID
	if id == "" {
		return "", fmt.Errorf("slide %s has no speaker-notes shape", slideID)
	}
	return id, nil
}

// slidesFromTemplate copies a deck through Drive, then substitutes placeholders.
// Two calls, because Slides itself cannot duplicate a presentation.
func slidesFromTemplate(ctx context.Context, token string, d FlowNodeData, sub func(string) string) (string, error) {
	templateID := sub(d.SlidesTemplateId)
	if templateID == "" {
		return "", fmt.Errorf("create_from_template needs a template presentation ID")
	}
	copied, err := googleCall(ctx, token, http.MethodPost,
		"https://www.googleapis.com/drive/v3/files/"+templateID+"/copy?fields=id,name,webViewLink",
		map[string]any{"name": firstNonEmpty(sub(d.SlidesTitle), "Copy of presentation")})
	if err != nil {
		return "", err
	}
	var file struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		WebViewLink string `json:"webViewLink"`
	}
	if json.Unmarshal([]byte(copied), &file) != nil || file.ID == "" {
		return "", fmt.Errorf("could not copy the template presentation")
	}

	if repl := strings.TrimSpace(sub(d.SlidesReplacements)); repl != "" {
		var pairs map[string]string
		if json.Unmarshal([]byte(repl), &pairs) != nil {
			return "", fmt.Errorf(`replacements must be a JSON object, e.g. {"{{name}}":"Jane"}`)
		}
		requests := make([]any, 0, len(pairs))
		for find, replace := range pairs {
			requests = append(requests, map[string]any{"replaceAllText": map[string]any{
				"containsText": map[string]any{"text": find, "matchCase": true},
				"replaceText":  replace,
			}})
		}
		if len(requests) > 0 {
			if _, err := slidesBatch(ctx, token, file.ID, requests); err != nil {
				return "", err
			}
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"presentationId": file.ID, "title": file.Name, "url": file.WebViewLink,
	})
	return string(payload), nil
}
