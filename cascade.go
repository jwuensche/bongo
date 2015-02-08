package bongo

import (
	"errors"
	"github.com/maxwellhealth/dotaccess"
	"github.com/maxwellhealth/mgo"
	"github.com/maxwellhealth/mgo/bson"
	"github.com/oleiade/reflections"
	"strings"
)

// Relation types (one-to-many or one-to-one)
const (
	REL_MANY = iota
	REL_ONE  = iota
)

type ReferenceField struct {
	BsonName string
	Value    interface{}
}

// Configuration to tell Bongo how to cascade data to related documents on save or delete
type CascadeConfig struct {
	// The collection to cascade to
	Collection *Collection

	// The relation type (does the target doc have an array of these docs [REL_MANY] or just reference a single doc [REL_ONE])
	RelType int

	// The property on the related doc to populate
	ThroughProp string

	// The query to find related docs
	Query bson.M

	// The data that constructs the query may have changed - this is to remove self from previous relations
	OldQuery bson.M

	// Should it also cascade the related doc on save?
	Nest bool

	// Data to cascade. Can be in dot notation
	Properties []string

	// An instance of the related doc if it needs to be nested
	Instance interface{}

	// If this is true, then just run the "remove" parts of the queries, instead of the remove + add
	RemoveOnly bool

	// If this is provided, use this field instead of _id for determining "sameness". This must also be a bson.ObjectId field
	ReferenceQuery []*ReferenceField

	// If you want to filter the data right before it's cascaded to related documents, use this.
	Filters []CascadeFilter
}

type CascadeFilter func(data map[string]interface{})

// Cascades a document's properties to related documents, after it has been prepared
// for db insertion (encrypted, etc)
func CascadeSave(collection *Collection, doc interface{}, preparedForSave map[string]interface{}) error {
	// Find out which properties to cascade
	if conv, ok := doc.(interface {
		GetCascade(*Collection) []*CascadeConfig
	}); ok {
		toCascade := conv.GetCascade(collection)
		for _, conf := range toCascade {

			if len(conf.ReferenceQuery) == 0 {
				conf.ReferenceQuery = []*ReferenceField{&ReferenceField{"_id", preparedForSave["_id"]}}
			}
			_, err := cascadeSaveWithConfig(conf, preparedForSave)
			if err != nil {
				return err
			}
			if conf.Nest {
				results := conf.Collection.Find(conf.Query)

				for results.Next(conf.Instance) {
					prepared := conf.Collection.PrepDocumentForSave(conf.Instance)
					err = CascadeSave(conf.Collection, conf.Instance, prepared)
					if err != nil {
						return err
					}
				}

			}
		}
	}
	return nil
}

// Deletes references to a document from its related documents
func CascadeDelete(collection *Collection, doc interface{}) {
	// Find out which properties to cascade
	if conv, ok := doc.(interface {
		GetCascade(*Collection) []*CascadeConfig
	}); ok {
		toCascade := conv.GetCascade(collection)

		// Get the ID

		for _, conf := range toCascade {
			if len(conf.ReferenceQuery) == 0 {
				id, err := reflections.GetField(doc, "Id")
				if err != nil {
					panic(err)
				}
				conf.ReferenceQuery = []*ReferenceField{&ReferenceField{"_id", id}}
			}

			cascadeDeleteWithConfig(conf)

		}

	}
}

// Runs a cascaded delete operation with one configuration
func cascadeDeleteWithConfig(conf *CascadeConfig) (*mgo.ChangeInfo, error) {

	switch conf.RelType {
	case REL_ONE:
		update := map[string]map[string]interface{}{
			"$set": map[string]interface{}{},
		}

		if len(conf.ThroughProp) > 0 {
			update["$set"][conf.ThroughProp] = nil
		} else {
			for _, p := range conf.Properties {
				update["$set"][p] = nil
			}
		}

		return conf.Collection.Collection().UpdateAll(conf.Query, update)
	case REL_MANY:
		update := map[string]map[string]interface{}{
			"$pull": map[string]interface{}{},
		}

		q := bson.M{}
		for _, f := range conf.ReferenceQuery {
			q[f.BsonName] = f.Value
		}
		update["$pull"][conf.ThroughProp] = q
		return conf.Collection.Collection().UpdateAll(conf.Query, update)
	}

	return &mgo.ChangeInfo{}, errors.New("Invalid relation type")
}

// Runs a cascaded save operation with one configuration
func cascadeSaveWithConfig(conf *CascadeConfig, preparedForSave map[string]interface{}) (*mgo.ChangeInfo, error) {
	// Create a new map with just the props to cascade

	data := make(map[string]interface{})

	for _, prop := range conf.Properties {
		split := strings.Split(prop, ".")

		if len(split) == 1 {
			data[prop] = preparedForSave[prop]
		} else {
			actualProp := split[len(split)-1]
			split := append([]string{}, split[:len(split)-1]...)
			curData := data

			for _, s := range split {
				if _, ok := curData[s]; ok {
					if mapped, ok := curData[s].(map[string]interface{}); ok {
						curData = mapped
					} else {
						panic("Cannot access non-map property via dot notationa")
					}

				} else {
					curData[s] = make(map[string]interface{})
					if mapped, ok := curData[s].(map[string]interface{}); ok {
						curData = mapped
					} else {
						panic("Cannot access non-map property via dot notationb")
					}
				}
			}

			val, _ := dotaccess.Get(preparedForSave, prop)
			if bsonId, ok := val.(bson.ObjectId); ok {
				if !bsonId.Valid() {
					curData[actualProp] = ""
					continue
				}
			}
			curData[actualProp] = val
		}

	}

	for _, f := range conf.Filters {
		f(data)
	}

	switch conf.RelType {
	case REL_ONE:
		if len(conf.OldQuery) > 0 {

			update1 := map[string]map[string]interface{}{
				"$set": map[string]interface{}{},
			}

			if len(conf.ThroughProp) > 0 {
				update1["$set"][conf.ThroughProp] = nil
			} else {
				for _, p := range conf.Properties {
					update1["$set"][p] = nil
				}
			}

			ret, err := conf.Collection.Collection().UpdateAll(conf.OldQuery, update1)

			if conf.RemoveOnly {
				return ret, err
			}
		}

		update := map[string]map[string]interface{}{
			"$set": map[string]interface{}{},
		}

		if len(conf.ThroughProp) > 0 {
			update["$set"][conf.ThroughProp] = data
		} else {
			for k, v := range data {
				update["$set"][k] = v
			}
		}

		// Just update
		return conf.Collection.Collection().UpdateAll(conf.Query, update)
	case REL_MANY:

		update1 := map[string]map[string]interface{}{
			"$pull": map[string]interface{}{},
		}

		q := bson.M{}
		for _, f := range conf.ReferenceQuery {
			q[f.BsonName] = f.Value
		}
		update1["$pull"][conf.ThroughProp] = q

		if len(conf.OldQuery) > 0 {
			ret, err := conf.Collection.Collection().UpdateAll(conf.OldQuery, update1)
			if conf.RemoveOnly {
				return ret, err
			}
		}

		// Remove self from current relations, so we can replace it
		conf.Collection.Collection().UpdateAll(conf.Query, update1)

		update2 := map[string]map[string]interface{}{
			"$push": map[string]interface{}{},
		}

		update2["$push"][conf.ThroughProp] = data

		return conf.Collection.Collection().UpdateAll(conf.Query, update2)

	}

	return &mgo.ChangeInfo{}, errors.New("Invalid relation type")

}
