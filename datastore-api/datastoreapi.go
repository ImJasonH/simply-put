package datastoreapi

// TODO: Support other request/response formats besides JSON (e.g., xml, gob)
// TODO: Figure out if PropertyList can support nested objects, or fail if they are detected.
// TODO: Add rudimentary single-property queries, pagination, sorting, etc.

import (
	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	idKey        = "_id"
	createdKey   = "_created"
	kindKey      = "_kind"
	updatedKey   = "_updated"
	defaultLimit = 10
)

func init() {
	http.HandleFunc("/datastore/v1dev/objects/", datastoreApi)
}

type UserQuery struct {
	Limit, Offset                      int
	FilterKey, FilterType, FilterValue string
	Cursor                             string
}

type UserInfo struct {
	Id string
}

// getUserId gets the Google User ID for an access token.
func getUserId(accessToken string, client http.Client) (id string, err error) {
	resp, err := client.Get("https://www.googleapis.com/oauth2/v1/userinfo?access_token=" + accessToken)
	if err != nil {
		return
	}
	var info UserInfo
	if err = json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return
	}
	id = info.Id
	if id == "" {
		err = errors.New("Invalid auth")
	}
	return
}

// getKindAndId parses the kind and ID from a request path.
func getKindAndId(path string) (kind string, id int64, err error) {
	var match bool
	if match, err = regexp.MatchString("/datastore/v1dev/objects/[a-zA-Z]+/[0-9]+", path); err != nil {
		return
	} else if match {
		kind = path[len("/datastore/v1dev/objects/"):strings.LastIndex(path, "/")]
		idStr := path[strings.LastIndex(path, "/")+1:]
		id, err = strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return
		}
		return
	}
	if match, err = regexp.MatchString("/datastore/v1dev/objects/[a-zA-Z]+", path); err != nil {
		return
	} else if match {
		kind = path[len("/datastore/v1dev/objects/"):]
		return
	}
	err = errors.New("Invalid path")
	return
}

// datastoreApi dispatches requests to the relevant API method and arranges certain common state
func datastoreApi(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	w.Header().Add("Access-Control-Allow-Origin", "*")

	r.ParseForm()
	client := urlfetch.Client(c)

	// Get the access_token from the request and turn it into a user ID with which we will namespace Kinds in the datastore.
	accessToken := r.Form.Get("access_token")
	if accessToken == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	userId, err := getUserId(accessToken, *client)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	kind, id, err := getKindAndId(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	dsKind := fmt.Sprintf("%s--%s", userId, kind)

	if id == 0 {
		switch r.Method {
		case "POST":
			insert(w, dsKind, r.Body, c)
			return
		case "GET":
			// TODO: Parse user request into UserQuery and pass to list method
			list(w, dsKind, UserQuery{}, c)
			return
		}
	} else {
		switch r.Method {
		case "GET":
			get(w, dsKind, id, c)
			return
		case "DELETE":
			delete(w, dsKind, id, c)
			return
		case "POST":
			// This is strictly "replace all properties/values", not "add new properties, update existing"
			update(w, dsKind, id, r.Body, c)
			return
		}
	}
	http.Error(w, "Unsupported Method", http.StatusMethodNotAllowed)
}

func delete(w http.ResponseWriter, kind string, id int64, c appengine.Context) {
	k := datastore.NewKey(c, kind, "", id, nil)
	if err := datastore.Delete(c, k); err != nil {
		if err == datastore.ErrNoSuchEntity {
			http.Error(w, "Not Found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
}

func get(w http.ResponseWriter, kind string, id int64, c appengine.Context) {
	k := datastore.NewKey(c, kind, "", id, nil)
	var plist datastore.PropertyList
	if err := datastore.Get(c, k, &plist); err != nil {
		if err == datastore.ErrNoSuchEntity {
			http.Error(w, "Not Found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	m := plistToMap(plist, k)
	m[idKey] = k.IntID()
	m[kindKey] = kind
	json.NewEncoder(w).Encode(m)
}

func insert(w http.ResponseWriter, kind string, r io.Reader, c appengine.Context) {
	var m map[string]interface{}
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m[createdKey] = time.Now()

	plist := mapToPlist(m)

	k := datastore.NewIncompleteKey(c, kind, nil)
	k, err := datastore.Put(c, k, &plist)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m[idKey] = k.IntID()
	m[kindKey] = kind
	json.NewEncoder(w).Encode(m)
}

// plistToMap transforms a PropertyList such as you would get from the datastore into a map[string]interface{} suitable for JSON-encoding.
func plistToMap(plist datastore.PropertyList, k *datastore.Key) map[string]interface{} {
	m := make(map[string]interface{})
	for _, p := range plist {
		if _, exists := m[p.Name]; exists {
			if _, isArr := m[p.Name].([]interface{}); isArr {
				m[p.Name] = append(m[p.Name].([]interface{}), p.Value)
			} else {
				m[p.Name] = []interface{}{m[p.Name], p.Value}
			}
		} else {
			m[p.Name] = p.Value
		}
	}
	m[idKey] = k.IntID()
	return m
}

// mapToPlist transforms a map[string]interface{} such as you would get from decoding JSON into a PropertyList to store in the datastore.
func mapToPlist(m map[string]interface{}) datastore.PropertyList {
	plist := make(datastore.PropertyList, 0, len(m))
	for k, v := range m {
		if _, mult := v.([]interface{}); mult {
			for _, mv := range v.([]interface{}) {
				plist = append(plist, datastore.Property{
					Name:     k,
					Value:    mv,
					Multiple: true,
				})
			}
		} else {
			plist = append(plist, datastore.Property{
				Name:  k,
				Value: v,
			})
		}
	}
	return plist
}

func list(w http.ResponseWriter, kind string, uq UserQuery, c appengine.Context) {
	limit := 3
	q := datastore.NewQuery(kind).Limit(limit)

	items := make([]map[string]interface{}, 0, limit)

	var crs datastore.Cursor
	for t := q.Run(c); ; {
		var plist datastore.PropertyList
		k, err := t.Next(&plist)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m := plistToMap(plist, k)
		m[kindKey] = kind
		items = append(items, m)
		if crs, err = t.Cursor(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	r := map[string]interface{}{
		"items":          items,
		"nextStartToken": crs.String(),
	}
	json.NewEncoder(w).Encode(r)
}

func update(w http.ResponseWriter, kind string, id int64, r io.Reader, c appengine.Context) {
	var m map[string]interface{}
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m[updatedKey] = time.Now()

	plist := mapToPlist(m)

	k := datastore.NewKey(c, kind, "", id, nil)
	if _, err := datastore.Put(c, k, &plist); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m[idKey] = id
	m[kindKey] = kind
	json.NewEncoder(w).Encode(m)
}