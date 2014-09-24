package simplyput

// TODO: Move metadata into single top-level "_meta" field to futureproof
// TODO: Add memcache
// TODO: Support ETags, If-Modified-Since, etc. (http://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html)
// TODO: PUT requests
// TODO: HEAD requests
// TODO: PATCH requests/semantics
// TODO: Batch requests (via multipart?)
// TODO: User POSTs a JSON schema, future requests are validated against that schema. Would anybody use that?
// TODO: Add end-to-end tests with appengine/aetest and/or net/http/httptest
// TODO: Partial responses using ?fields= param (https://developers.google.com/+/api/#partial-responses)

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"
)

const (
	idKey        = "_id"
	createdKey   = "_created"
	updatedKey   = "_updated"
	defaultLimit = 10
)

var nowFunc = time.Now

var invalidPath = errors.New("invalid path")

func init() {
	http.HandleFunc("/", handleHTTP)
}

type filter struct {
	Key, Value string
}
type userQuery struct {
	Limit                        int
	StartCursor, EndCursor, Sort string
	Filters                      []filter
}

// getUserID gets the Google User ID for an access token.
func getUserID(accessToken string, client http.Client) (string, error) {
	resp, err := client.Get("https://www.googleapis.com/oauth2/v1/userinfo?access_token=" + accessToken)
	if err != nil {
		return "", err
	}
	var info struct {
		ID string
	}
	if err = json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	resp.Body.Close()
	id := info.ID
	if id == "" {
		return "", errors.New("invalid auth")
	}
	return id, nil
}

// getKindAndID parses the kind and ID from a request path.
func getKindAndID(path string) (string, int64, error) {
	if !strings.HasPrefix(path, "/") || path == "/" {
		return "", int64(0), invalidPath
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) > 2 {
		return "", int64(0), invalidPath
	} else if len(parts) == 1 {
		return parts[0], int64(0), nil
	} else if len(parts) == 2 {
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return "", int64(0), err
		}
		return parts[0], id, nil
	}
	return "", int64(0), invalidPath
}

// handle dispatches requests to the relevant API method and arranges certain common state
func handleHTTP(w http.ResponseWriter, r *http.Request) {
	handle(appengine.NewContext(r), w, r)
}

func handle(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")

	r.ParseForm()
	client := urlfetch.Client(c)

	var userID string
	if appengine.IsDevAppServer() {
		// For local development, don't require an access token or user ID
		// If the user_id param is set, that's the user ID.
		userID = r.Form.Get("user_id")
	} else {
		// Get the access_token from the request and turn it into a user ID with which we will namespace Kinds in the datastore.
		accessToken := r.Form.Get("access_token")
		if accessToken == "" {
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, "Bearer ") {
				accessToken = h[len("Bearer "):]
			}
		}
		if accessToken == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		var err error // Needed because otherwise the next line shadows userID...
		userID, err = getUserID(accessToken, *client)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	kind, id, err := getKindAndID(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	dsKind := fmt.Sprintf("%s--%s", userID, kind)

	var resp map[string]interface{}
	errCode := http.StatusOK
	if id == int64(0) {
		switch r.Method {
		case "POST":
			resp, errCode = insert(c, dsKind, r.Body)
			r.Body.Close()
		case "GET":
			uq, err := newUserQuery(r)
			if err != nil {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			resp, errCode = list(c, dsKind, *uq)
		default:
			http.Error(w, "Unsupported Method", http.StatusMethodNotAllowed)
			return
		}
	} else {
		switch r.Method {
		case "GET":
			resp, errCode = get(c, dsKind, id)
		case "DELETE":
			errCode = delete2(c, dsKind, id)
		case "POST":
			// This is strictly "replace all properties/values", not "add new properties, update existing"
			resp, errCode = update(c, dsKind, id, r.Body)
			r.Body.Close()
		default:
			http.Error(w, "Unsupported Method", http.StatusMethodNotAllowed)
			return
		}
	}
	if errCode != http.StatusOK {
		http.Error(w, "", errCode)
		return
	}
	if resp != nil && len(resp) != 0 {
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Add("Content-Type", "application/json")
}

func newUserQuery(r *http.Request) (*userQuery, error) {
	uq := userQuery{
		StartCursor: r.FormValue("start"),
		EndCursor:   r.FormValue("end"),
		Sort:        r.FormValue("sort"),
	}
	if r.FormValue("limit") != "" {
		lim, err := strconv.Atoi(r.FormValue("limit"))
		if err != nil {
			return nil, err
		}
		uq.Limit = lim
	}

	for _, f := range map[string][]string(r.Form)["where"] {
		parts := strings.Split(f, "=")
		if len(parts) != 2 {
			return nil, errors.New("invalid where: " + f)
		}
		uq.Filters = append(uq.Filters, filter{Key: parts[0], Value: parts[1]})
	}
	return &uq, nil
}

func delete2(c appengine.Context, kind string, id int64) int {
	k := datastore.NewKey(c, kind, "", id, nil)
	if err := datastore.Delete(c, k); err != nil {
		if err == datastore.ErrNoSuchEntity {
			return http.StatusNotFound
		} else {
			c.Errorf("%v", err)
			return http.StatusInternalServerError
		}
	}
	return http.StatusOK
}

func get(c appengine.Context, kind string, id int64) (map[string]interface{}, int) {
	k := datastore.NewKey(c, kind, "", id, nil)
	var pl datastore.PropertyList
	if err := datastore.Get(c, k, &pl); err != nil {
		if err == datastore.ErrNoSuchEntity {
			return nil, http.StatusNotFound
		}
		c.Errorf("%v", err)
		return nil, http.StatusInternalServerError
	}
	m := plistToMap(pl, k.IntID())
	m[idKey] = k.IntID()
	return m, http.StatusOK
}

func insert(c appengine.Context, kind string, r io.Reader) (map[string]interface{}, int) {
	m, err := fromJSON(c, r)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	m[createdKey] = nowFunc().Unix()

	pl := mapToPlist("", m)

	k := datastore.NewIncompleteKey(c, kind, nil)
	k, err = datastore.Put(c, k, &pl)
	if err != nil {
		c.Errorf("%v", err)
		return nil, http.StatusInternalServerError
	}
	m[idKey] = int64(k.IntID())
	return m, http.StatusOK
}

// plistToMap transforms a plist such as you would get from the datastore into a map[string]interface{} suitable for JSON-encoding.
func plistToMap(pl datastore.PropertyList, id int64) map[string]interface{} {
	m := make(map[string]interface{})
	for _, p := range pl {
		parts := strings.Split(p.Name, ".")
		sub := m
		for _, p := range parts[:len(parts)-1] {
			// Traverse the path up until the leaf
			if i, exists := sub[p]; exists {
				// Already seen this path, traverse it
				if ii, ok := i.(map[string]interface{}); ok {
					sub = ii
				} else {
					// Got a sub-property of a non-map property. Uh oh...
					// Not sure it's worth failing/logging though...
				}
			} else {
				// First time down this path, add a new empty map
				next := map[string]interface{}{}
				sub[p] = next
				sub = next
			}
		}
		leaf := parts[len(parts)-1]
		if _, exists := sub[leaf]; exists {
			if !p.Multiple {
				// We would expect p.Multiple to be true here.
				// Not sure it's worth failing/logging though...
			}
			if _, isArr := sub[leaf].([]interface{}); isArr {
				// Already an array here, append to it
				sub[leaf] = append(sub[leaf].([]interface{}), p.Value)
			} else {
				// Already a single value here, should be an array now.
				sub[leaf] = []interface{}{sub[leaf], p.Value}
			}
		} else {
			sub[leaf] = p.Value
		}
	}
	m[idKey] = id
	return m
}

// mapToPlist transforms a map[string]interface{} such as you would get from decoding JSON into a datastore.PropertyList to store in the datastore.
func mapToPlist(prefix string, m map[string]interface{}) datastore.PropertyList {
	pl := make(datastore.PropertyList, 0, len(m))
	for k, v := range m {
		if m, nest := v.(map[string]interface{}); nest {
			// Generate a datastore.PropertyList for this sub-map, and append it
			pl = append(pl, mapToPlist(prefix+k+".", m)...)
		} else if _, mult := v.([]interface{}); mult {
			// Generate a prop for every item in the slice
			for _, mv := range v.([]interface{}) {
				pl = append(pl, datastore.Property{
					Name:     prefix + k,
					Value:    mv,
					Multiple: true,
				})
			}
			// TODO: Apparently no way to store an empty list? That seems odd...
		} else {
			pl = append(pl, datastore.Property{
				Name:  prefix + k,
				Value: v,
			})
		}
	}
	return pl
}

func list(c appengine.Context, kind string, uq userQuery) (map[string]interface{}, int) {
	q := datastore.NewQuery(kind)

	if uq.Limit != 0 {
		q = q.Limit(uq.Limit)
	}
	if uq.Sort != "" {
		q = q.Order(uq.Sort)
	}
	if c, err := datastore.DecodeCursor(uq.StartCursor); err == nil {
		q = q.Start(c)
	}
	if c, err := datastore.DecodeCursor(uq.EndCursor); err == nil {
		q = q.End(c)
	}
	// TODO: Support numerical filters, not just strings
	for _, f := range uq.Filters {
		q = q.Filter(f.Key, f.Value)
	}

	items := make([]map[string]interface{}, 0)

	var crs datastore.Cursor
	for t := q.Run(c); ; {
		var pl datastore.PropertyList
		k, err := t.Next(&pl)
		if err == datastore.Done {
			break
		}
		if err != nil {
			c.Errorf("%v", err)
			return nil, http.StatusInternalServerError
		}
		m := plistToMap(pl, k.IntID())
		items = append(items, m)
		if crs, err = t.Cursor(); err != nil {
			c.Errorf("%v", err)
			return nil, http.StatusInternalServerError
		}
	}
	r := map[string]interface{}{
		"items":          items,
		"nextStartToken": crs.String(),
	}
	return r, http.StatusOK
}

func update(c appengine.Context, kind string, id int64, r io.Reader) (map[string]interface{}, int) {
	// TODO: Get the entity, if it's found, set the _created time accordingly.

	m, err := fromJSON(c, r)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	delete(m, createdKey) // Ignore any _created value the user provides
	delete(m, idKey)      // Ignore any _id value the user provides
	m[updatedKey] = nowFunc().Unix()

	pl := mapToPlist("", m)

	k := datastore.NewKey(c, kind, "", id, nil)
	if _, err := datastore.Put(c, k, &pl); err != nil {
		c.Errorf("%v", err)
		return nil, http.StatusInternalServerError
	}
	m[idKey] = id
	return m, http.StatusOK
}

func fromJSON(c appengine.Context, r io.Reader) (map[string]interface{}, error) {
	var m map[string]interface{}
	err := json.NewDecoder(r).Decode(&m)
	if err != nil {
		c.Errorf("decoding json: %v", err)
	}
	return m, err
}
