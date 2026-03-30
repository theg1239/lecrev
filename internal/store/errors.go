package store

import "errors"

var ErrAlreadyExists = errors.New("record already exists")
var ErrNotFound = errors.New("record not found")
var ErrAccessDenied = errors.New("access denied")
