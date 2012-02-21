/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package index

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"camlistore.org/pkg/blobserver"
	"camlistore.org/pkg/jsonconfig"

	"camlistore.org/third_party/launchpad.net/mgo"
	"camlistore.org/third_party/launchpad.net/mgo/bson"
)

// We explicitely separate the key and the value in a document,
// instead of simply storing as key:value, to avoid problems
// such as "." being an illegal char in a key name. Also because
// there is no way to do partial matching for key names (one can
// only check for their existence with bson.M{$exists: true}).
const (
	collectionName = "keys"
	mgoKey         = "key"
	mgoValue       = "value"
)

type MongoWrapper struct {
	Servers    string
	User       string
	Password   string
	Database   string
	Collection string
}

// Note that Ping won't work with old (1.2) mongo servers.
func (mgw *MongoWrapper) TestConnection(timeout int64) bool {
	session, err := mgo.Dial(mgw.Servers)
	if err != nil {
		return false
	}
	defer session.Close()
	t := time.Duration(timeout)
	session.SetSyncTimeout(t)
	if err = session.Ping(); err != nil {
		return false
	}
	return true
}

func (mgw *MongoWrapper) getConnection() (*mgo.Session, error) {
	// TODO(mpl): do some "client caching" as in mysql, to avoid systematically dialing?
	session, err := mgo.Dial(mgw.Servers)
	if err != nil {
		return nil, err
	}
	session.SetMode(mgo.Monotonic, true)
	session.SetSafe(&mgo.Safe{})
	return session, nil
}

// TODO(mpl): I'm only calling getCollection at the beginning, and 
// keeping the collection around and reusing it everywhere, instead
// of calling getCollection everytime, because that's the easiest.
// But I can easily change that. Gustavo says it does not make 
// much difference either way.
// Brad, what do you think?
func (mgw *MongoWrapper) getCollection() (*mgo.Collection, error) {
	session, err := mgw.getConnection()
	if err != nil {
		return nil, err
	}
	session.SetSafe(&mgo.Safe{})
	session.SetMode(mgo.Strong, true)
	c := session.DB(mgw.Database).C(mgw.Collection)
	return c, nil
}

func init() {
	blobserver.RegisterStorageConstructor("mongodbindexer",
		blobserver.StorageConstructor(newMongoIndexFromConfig))
}

func newMongoIndex(mgw *MongoWrapper) (*Index, error) {
	db, err := mgw.getCollection()
	if err != nil {
		return nil, err
	}
	mongoStorage := &mongoKeys{db: db}
	return New(mongoStorage), nil
}

func newMongoIndexFromConfig(ld blobserver.Loader, config jsonconfig.Obj) (blobserver.Storage, error) {
	blobPrefix := config.RequiredString("blobSource")
	mgw := &MongoWrapper{
		Servers:    config.OptionalString("servers", "localhost"),
		Database:   config.RequiredString("database"),
		Collection: collectionName,
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	sto, err := ld.GetStorage(blobPrefix)
	if err != nil {
		return nil, err
	}

	ix, err := newMongoIndex(mgw)
	if err != nil {
		return nil, err
	}
	ix.BlobSource = sto

	// Good enough, for now:
	ix.KeyFetcher = ix.BlobSource

	if wipe := os.Getenv("CAMLI_MONGO_WIPE"); wipe != "" {
		dowipe, err := strconv.ParseBool(wipe)
		if err != nil {
			return nil, err
		}
		if dowipe {
			err = ix.s.Delete("")
			if err != nil {
				return nil, err
			}
		}
	}

	return ix, err
}

// Implementation of index Iterator
type mongoStrIterator struct {
	res bson.M
	*mgo.Iter
}

func (s mongoStrIterator) Next() bool {
	return s.Iter.Next(&s.res)
}

func (s mongoStrIterator) Key() (key string) {
	key, ok := (s.res[mgoKey]).(string)
	if !ok {
		return ""
	}
	return key
}

func (s mongoStrIterator) Value() (value string) {
	value, ok := (s.res[mgoValue]).(string)
	if !ok {
		return ""
	}
	return value
}

func (s mongoStrIterator) Close() error {
	// TODO(mpl): think about anything more to be done here.
	return nil
}

// Implementation of IndexStorage
type mongoKeys struct {
	mu sync.Mutex // guards db
	db *mgo.Collection
}

func (mk *mongoKeys) Get(key string) (string, error) {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	res := bson.M{}
	q := mk.db.Find(&bson.M{mgoKey: key})
	err := q.One(&res)
	if err != nil {
		if err == mgo.NotFound {
			return "", ErrNotFound
		} else {
			return "", err
		}
	}
	return res[mgoValue].(string), err
}

func (mk *mongoKeys) Find(key string) Iterator {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	// TODO(mpl): escape other special chars, or maybe replace $regex with something
	// more suited if possible.
	cleanedKey := strings.Replace(key, "|", `\|`, -1)
	iter := mk.db.Find(&bson.M{mgoKey: &bson.M{"$regex": "^" + cleanedKey}}).Sort(&bson.M{mgoKey: 1}).Iter()
	return mongoStrIterator{res: bson.M{}, Iter: iter}
}

func (mk *mongoKeys) Set(key, value string) error {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	_, err := mk.db.Upsert(&bson.M{mgoKey: key}, &bson.M{mgoKey: key, mgoValue: value})
	return err
}

// Delete removes the document with the matching key.
// If key is "", it removes all documents.
func (mk *mongoKeys) Delete(key string) error {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	if key == "" {
		return mk.db.RemoveAll(nil)
	}
	return mk.db.Remove(&bson.M{mgoKey: key})
}

func (mk *mongoKeys) BeginBatch() BatchMutation {
	return &batch{}
}

func (mk *mongoKeys) CommitBatch(bm BatchMutation) error {
	b, ok := bm.(*batch)
	if !ok {
		return errors.New("invalid batch type; not an instance returned by BeginBatch")
	}
	mk.mu.Lock()
	defer mk.mu.Unlock()
	for _, m := range b.m {
		if m.delete {
			if err := mk.db.Remove(bson.M{mgoKey: m.key}); err != nil {
				return err
			}
		} else {
			if _, err := mk.db.Upsert(&bson.M{mgoKey: m.key}, &bson.M{mgoKey: m.key, mgoValue: m.value}); err != nil {
				return err
			}
		}
	}
	return nil
}
