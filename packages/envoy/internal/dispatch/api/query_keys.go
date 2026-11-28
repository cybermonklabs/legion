package api

import (
	"net/http"
	"net/url"
	"reflect"
)

// The four functions below read a route's query struct and refuse a key named twice. They are the
// same four that cybermonklabs/legion#3976 adds in evals.go, where they also guard the eval and
// delivery reads; this branch needs them alone, for the one layer every route runs
// (refuseRepeatedQueryKeys, server.go), so it carries them here rather than all of that pull
// request. When #3976 reaches main, package api holds them twice and does not build: the merge
// of main that brings #3976 deletes this file.

// queryKeysOf is the keys params, a route's query struct, declares: each field's `query` tag.
func queryKeysOf(params any) []string {
	structure := reflect.TypeOf(params)
	keys := make([]string, 0, structure.NumField())
	for index := range structure.NumField() {
		if key, tagged := structure.Field(index).Tag.Lookup("query"); tagged {
			keys = append(keys, key)
		}
	}
	return keys
}

// oneValueQueryKeys is the keys params, a route's query struct, declares for one value: the
// `query` tag of each field that is no list, in the order params declares them. A list field is a
// key the route takes any number of values of.
func oneValueQueryKeys(params any) []string {
	structure := reflect.TypeOf(params)
	keys := make([]string, 0, structure.NumField())
	for index := range structure.NumField() {
		field := structure.Field(index)
		if key, tagged := field.Tag.Lookup("query"); tagged && field.Type.Kind() != reflect.Slice {
			keys = append(keys, key)
		}
	}
	return keys
}

// repeatedQueryKey refuses a query naming more than once a key its route takes one value of: 400
// INVALID_QUERY naming the first such key in the order oneValue gives them, nil where none is.
// Such a key is read as its first value, so a second value would be answered as a question other
// than the one asked. A list key of the route's query struct is taken any number of times.
func repeatedQueryKey(query url.Values, oneValue []string) error {
	for _, key := range oneValue {
		if err := namedOnce(query, key); err != nil {
			return err
		}
	}
	return nil
}

// namedOnce refuses a query naming key more than once, for a route that reads one value of it:
// 400 INVALID_QUERY naming the key and how many times the query names it, nil where it names the
// key once or not at all.
func namedOnce(query url.Values, key string) error {
	if named := len(query[key]); named > 1 {
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "%q is named %d times, and this route reads one value of it", key, named)
	}
	return nil
}
