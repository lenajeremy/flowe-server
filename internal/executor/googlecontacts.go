package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Contacts, via the People API.
//
// Three things about this API shape the ops, and each one is a 400 rather than a
// graceful degradation if ignored:
//
//   - Reads return nothing useful unless a field mask is supplied. personFields on
//     get/list, readMask on search and otherContacts. There is no sensible default
//     upstream, so contactFields supplies one here.
//   - updateContact is optimistically concurrent. It needs updatePersonFields *and*
//     the etag from a prior read, or it fails with failedPrecondition. So
//     update_contact reads first and replays the etag, which also means it will
//     refuse rather than clobber a contact edited since.
//   - searchContacts runs against a cache that has to be warmed with an empty
//     query before it returns anything. A first search otherwise looks like "no
//     results" rather than "not ready", so the op warms it and retries.

const peopleAPI = "https://people.googleapis.com/v1"

// contactFields is the default field mask: enough to be useful in a workflow
// without pulling every photo and relation.
const contactFields = "names,emailAddresses,phoneNumbers,organizations,addresses,biographies,metadata"

// peopleResource normalizes a contact identifier to people/{id}.
func peopleResource(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "people/") {
		return v
	}
	return "people/" + v
}

func runGoogleContacts(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.ContactsLimit, 50)
	person := func() string { return peopleResource(sub(d.ContactsResourceName)) }
	fields := func() string { return firstNonEmpty(sub(d.ContactsFields), contactFields) }

	switch d.IntegrationOp {
	// ---- reading ----
	case "get_my_profile":
		return googleCall(ctx, token, http.MethodGet,
			peopleAPI+"/people/me?personFields="+url.QueryEscape(fields()), nil)

	case "list_contacts":
		q := url.Values{
			"personFields": {fields()},
			"pageSize":     {fmt.Sprint(limit)},
		}
		if v := sub(d.ContactsPageToken); v != "" {
			q.Set("pageToken", v)
		}
		if v := sub(d.ContactsSortOrder); v != "" {
			q.Set("sortOrder", v)
		}
		raw, err := googleCall(ctx, token, http.MethodGet,
			peopleAPI+"/people/me/connections?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "get_contact":
		if person() == "" {
			return "", fmt.Errorf("get_contact needs a contact resource name, e.g. people/c123")
		}
		return googleCall(ctx, token, http.MethodGet,
			peopleAPI+"/"+person()+"?personFields="+url.QueryEscape(fields()), nil)

	case "search_contacts":
		if sub(d.ContactsQuery) == "" {
			return "", fmt.Errorf("search_contacts needs a query")
		}
		return peopleSearch(ctx, token, "/people:searchContacts",
			sub(d.ContactsQuery), fields(), limit)

	case "list_other_contacts":
		// "Other contacts" are people you have corresponded with but never saved.
		q := url.Values{
			"readMask": {"names,emailAddresses,phoneNumbers"},
			"pageSize": {fmt.Sprint(limit)},
		}
		if v := sub(d.ContactsPageToken); v != "" {
			q.Set("pageToken", v)
		}
		raw, err := googleCall(ctx, token, http.MethodGet, peopleAPI+"/otherContacts?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "search_other_contacts":
		if sub(d.ContactsQuery) == "" {
			return "", fmt.Errorf("search_other_contacts needs a query")
		}
		return peopleSearch(ctx, token, "/otherContacts:search",
			sub(d.ContactsQuery), "names,emailAddresses,phoneNumbers", limit)

	// ---- writing ----
	case "create_contact":
		body, err := peopleContactBody(d, sub)
		if err != nil {
			return "", err
		}
		return googleCall(ctx, token, http.MethodPost,
			peopleAPI+"/people:createContact?personFields="+url.QueryEscape(fields()), body)

	case "update_contact":
		if person() == "" {
			return "", fmt.Errorf("update_contact needs a contact resource name")
		}
		return peopleUpdate(ctx, token, person(), fields(), d, sub)

	case "delete_contact":
		if person() == "" {
			return "", fmt.Errorf("delete_contact needs a contact resource name")
		}
		if _, err := googleCall(ctx, token, http.MethodDelete,
			peopleAPI+"/"+person()+":deleteContact", nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deleted":%q}`, person()), nil

	case "batch_delete_contacts":
		names := splitCSV(sub(d.ContactsResourceName))
		if len(names) == 0 {
			return "", fmt.Errorf("batch_delete_contacts needs at least one resource name")
		}
		for i, n := range names {
			names[i] = peopleResource(n)
		}
		if _, err := googleCall(ctx, token, http.MethodPost,
			peopleAPI+"/people:batchDeleteContacts",
			map[string]any{"resourceNames": names}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deleted":%d}`, len(names)), nil

	case "copy_other_contact":
		// Promotes an "other contact" into the real address book.
		if person() == "" {
			return "", fmt.Errorf("copy_other_contact needs an other-contact resource name")
		}
		return googleCall(ctx, token, http.MethodPost,
			peopleAPI+"/"+person()+":copyOtherContactToMyContactsGroup",
			map[string]any{"copyMask": "names,emailAddresses,phoneNumbers"})

	// ---- groups ----
	case "list_contact_groups":
		return googleCall(ctx, token, http.MethodGet,
			fmt.Sprintf("%s/contactGroups?pageSize=%d", peopleAPI, limit), nil)

	case "get_contact_group":
		if sub(d.ContactsGroupId) == "" {
			return "", fmt.Errorf("get_contact_group needs a group resource name")
		}
		return googleCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/%s?maxMembers=%d", peopleAPI, peopleGroup(sub(d.ContactsGroupId)), limit), nil)

	case "create_contact_group":
		if sub(d.ContactsGroupName) == "" {
			return "", fmt.Errorf("create_contact_group needs a name")
		}
		return googleCall(ctx, token, http.MethodPost, peopleAPI+"/contactGroups",
			map[string]any{"contactGroup": map[string]any{"name": sub(d.ContactsGroupName)}})

	case "update_contact_group":
		if sub(d.ContactsGroupId) == "" || sub(d.ContactsGroupName) == "" {
			return "", fmt.Errorf("update_contact_group needs a group resource name and a new name")
		}
		// Groups also carry an etag, so read it before writing.
		raw, err := googleCall(ctx, token, http.MethodGet,
			peopleAPI+"/"+peopleGroup(sub(d.ContactsGroupId)), nil)
		if err != nil {
			return "", err
		}
		etag := jsonField(raw, "etag")
		group := map[string]any{"name": sub(d.ContactsGroupName)}
		if etag != "" {
			group["etag"] = etag
		}
		return googleCall(ctx, token, http.MethodPut,
			peopleAPI+"/"+peopleGroup(sub(d.ContactsGroupId)),
			map[string]any{"contactGroup": group, "updateGroupFields": "name"})

	case "delete_contact_group":
		if sub(d.ContactsGroupId) == "" {
			return "", fmt.Errorf("delete_contact_group needs a group resource name")
		}
		return googleCall(ctx, token, http.MethodDelete,
			peopleAPI+"/"+peopleGroup(sub(d.ContactsGroupId))+"?deleteContacts=false", nil)

	case "modify_group_members":
		if sub(d.ContactsGroupId) == "" {
			return "", fmt.Errorf("modify_group_members needs a group resource name")
		}
		add, remove := splitCSV(sub(d.ContactsAddMembers)), splitCSV(sub(d.ContactsRemoveMembers))
		if len(add) == 0 && len(remove) == 0 {
			return "", fmt.Errorf("modify_group_members needs contacts to add or remove")
		}
		body := map[string]any{}
		if len(add) > 0 {
			for i, n := range add {
				add[i] = peopleResource(n)
			}
			body["resourceNamesToAdd"] = add
		}
		if len(remove) > 0 {
			for i, n := range remove {
				remove[i] = peopleResource(n)
			}
			body["resourceNamesToRemove"] = remove
		}
		return googleCall(ctx, token, http.MethodPost,
			peopleAPI+"/"+peopleGroup(sub(d.ContactsGroupId))+"/members:modify", body)

	case "":
		return "", fmt.Errorf("no Google Contacts operation selected")
	}
	return "", fmt.Errorf("unsupported Google Contacts operation: %s", d.IntegrationOp)
}

func peopleGroup(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "contactGroups/") {
		return v
	}
	return "contactGroups/" + v
}

// peopleSearch warms the search cache before querying. The People API documents
// that a search runs against a cache which an empty-query request populates, so a
// cold first search returns nothing rather than an error — indistinguishable from
// a genuine miss unless it is warmed.
func peopleSearch(ctx context.Context, token, path, query, readMask string, limit int) (string, error) {
	warm := url.Values{"query": {""}, "readMask": {readMask}}
	// A failure here is not fatal: the real query below still reports properly.
	_, _ = googleCall(ctx, token, http.MethodGet, peopleAPI+path+"?"+warm.Encode(), nil)

	q := url.Values{
		"query":    {query},
		"readMask": {readMask},
		"pageSize": {fmt.Sprint(limit)},
	}
	raw, err := googleCall(ctx, token, http.MethodGet, peopleAPI+path+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	return truncateStr(raw, 10000), nil
}

// peopleUpdate reads the contact to obtain its etag, then patches. The etag is
// what makes this safe: if the contact changed since the read, Google rejects the
// write rather than silently overwriting someone else's edit.
func peopleUpdate(ctx context.Context, token, resource, fields string,
	d FlowNodeData, sub func(string) string) (string, error) {

	current, err := googleCall(ctx, token, http.MethodGet,
		peopleAPI+"/"+resource+"?personFields="+url.QueryEscape(contactFields), nil)
	if err != nil {
		return "", err
	}
	etag := jsonField(current, "etag")
	if etag == "" {
		return "", fmt.Errorf("could not read the current version of %s, so it cannot be "+
			"updated safely", resource)
	}

	body, err := peopleContactBody(d, sub)
	if err != nil {
		return "", err
	}
	body["etag"] = etag

	// Only the fields actually supplied are named in the mask, so an omitted
	// field is left alone rather than cleared.
	mask := []string{}
	for _, f := range []string{"names", "emailAddresses", "phoneNumbers", "organizations",
		"addresses", "biographies"} {
		if _, ok := body[f]; ok {
			mask = append(mask, f)
		}
	}
	if len(mask) == 0 {
		return "", fmt.Errorf("update_contact needs at least one field to change")
	}
	q := url.Values{
		"updatePersonFields": {strings.Join(mask, ",")},
		"personFields":       {fields},
	}
	return googleCall(ctx, token, http.MethodPatch,
		peopleAPI+"/"+resource+":updateContact?"+q.Encode(), body)
}

// peopleContactBody assembles a person from the node's fields. Every People field
// is a list of objects rather than a scalar, which is why a single email address
// still arrives wrapped.
func peopleContactBody(d FlowNodeData, sub func(string) string) (map[string]any, error) {
	body := map[string]any{}
	if given, family := sub(d.ContactsGivenName), sub(d.ContactsFamilyName); given != "" || family != "" {
		name := map[string]any{}
		if given != "" {
			name["givenName"] = given
		}
		if family != "" {
			name["familyName"] = family
		}
		body["names"] = []any{name}
	}
	if v := splitCSV(sub(d.ContactsEmail)); len(v) > 0 {
		list := make([]any, 0, len(v))
		for _, e := range v {
			list = append(list, map[string]any{"value": e})
		}
		body["emailAddresses"] = list
	}
	if v := splitCSV(sub(d.ContactsPhone)); len(v) > 0 {
		list := make([]any, 0, len(v))
		for _, p := range v {
			list = append(list, map[string]any{"value": p})
		}
		body["phoneNumbers"] = list
	}
	if org, title := sub(d.ContactsOrganization), sub(d.ContactsJobTitle); org != "" || title != "" {
		o := map[string]any{}
		if org != "" {
			o["name"] = org
		}
		if title != "" {
			o["title"] = title
		}
		body["organizations"] = []any{o}
	}
	if v := sub(d.ContactsAddress); v != "" {
		body["addresses"] = []any{map[string]any{"formattedValue": v}}
	}
	if v := sub(d.ContactsNotes); v != "" {
		body["biographies"] = []any{map[string]any{"value": v, "contentType": "TEXT_PLAIN"}}
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("a contact needs at least a name, email address or phone number")
	}
	// Guard against a caller assuming JSON passthrough works here.
	if raw := strings.TrimSpace(sub(d.ContactsRawPerson)); raw != "" {
		var extra map[string]any
		if json.Unmarshal([]byte(raw), &extra) != nil {
			return nil, fmt.Errorf("extra fields must be a JSON object shaped like a People API person")
		}
		for k, v := range extra {
			body[k] = v
		}
	}
	return body, nil
}
