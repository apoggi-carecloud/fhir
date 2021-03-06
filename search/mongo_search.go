package search

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/eug48/fhir/models"
	"github.com/eug48/fhir/models2"
	mongowrapper "github.com/opencensus-integrations/gomongowrapper"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	moptions "go.mongodb.org/mongo-driver/mongo/options"
)

// This is a MongoDB internal error code for an interrupted operation, see:
// https://github.com/mongodb/mongo/blob/master/src/mongo/base/error_codes.err#L217
var opInterruptedCode = 11601

// BSONQuery is a BSON document constructed from the original string search query.
type BSONQuery struct {
	Resource string
	Query    bson.M
	Pipeline []bson.M
}

// NewBSONQuery initializes a new BSONQuery and returns a pointer to that BSONQuery.
func NewBSONQuery(resource string) *BSONQuery {
	return &BSONQuery{Resource: resource}
}

func (b *BSONQuery) usesPipeline() bool {
	return b.Query == nil
}

func (b *BSONQuery) DebugString() string {
	out := bytes.Buffer{}
	out.WriteString(fmt.Sprintf("Resource: %s; ", b.Resource))
	if b.Query != nil {
		out.WriteString("Query: ")
		queryJSON, err := bson.MarshalExtJSON(b.Query, true, false)
		if err != nil {
			out.WriteString(" BSONQuery.DebugString error: " + err.Error())
		}
		out.Write(queryJSON)
		out.WriteString("; ")
	}
	if b.Pipeline != nil {
		out.WriteString("Pipeline: ")
		doc := bson.M{"stages": b.Pipeline}
		pipelineJson, err := bson.MarshalExtJSON(doc, true, false)
		if err != nil {
			out.WriteString(" BSONQuery.DebugString error: " + err.Error())
		}
		out.Write(pipelineJson)
		out.WriteString("; ")
	}
	return out.String()
}

// CountCache is used to cache the total count of results for a specific query.
// The Id is the md5 hash of the query string.
type CountCache struct {
	Id    string `bson:"_id"`
	Count uint32 `bson:"count"`
}

// MongoSearcher implements FHIR searches using the Mongo database.
type MongoSearcher struct {
	db                           *mongowrapper.WrappedDatabase
	ctx                          context.Context
	client                       *mongo.Client // only non-nil for newly created sessions - Close() should be called
	session                      mongo.Session // only non-nil for newly created sessions - Close() should be called
	countTotalResults            bool
	enableCISearches             bool
	tokenParametersCaseSensitive bool
	readonly                     bool
}

// NewMongoSearcher creates a new instance of a MongoSearcher for an already open session
func NewMongoSearcher(db *mongowrapper.WrappedDatabase, ctx context.Context, countTotalResults, enableCISearches, tokenParametersCaseSensitive, readonly bool) *MongoSearcher {
	return &MongoSearcher{
		db:                           db,
		ctx:                          ctx,
		countTotalResults:            countTotalResults,
		enableCISearches:             enableCISearches,
		tokenParametersCaseSensitive: tokenParametersCaseSensitive,
		readonly:                     readonly,
	}
}

// NewMongoSearcher creates a new instance of a MongoSearcher with a new connection
// Call Close()
func NewMongoSearcherForUri(mongoUri string, mongoDatabaseName string, countTotalResults, enableCISearches, tokenParametersCaseSensitive, readonly bool) *MongoSearcher {

	client, err := mongowrapper.Connect(context.Background(), moptions.Client().ApplyURI(mongoUri))
	if err != nil {
		panic(errors.Wrap(err, "NewMongoSearcherForUri"))
	}

	session, err := client.StartSession()
	if err != nil {
		panic(errors.Wrap(err, "NewMongoSearcherForUri --> StartSession"))
	}

	db := client.Database(mongoDatabaseName)

	return &MongoSearcher{
		db:                           db,
		ctx:                          context.TODO(),
		session:                      session,
		countTotalResults:            countTotalResults,
		enableCISearches:             enableCISearches,
		tokenParametersCaseSensitive: tokenParametersCaseSensitive,
		readonly:                     readonly,
	}
}

// Close a MongoDB session opened by NewMongoSearcherForUri
func (m *MongoSearcher) Close() {
	if m.client != nil {
		m.session.EndSession(m.ctx)
		m.client.Disconnect(m.ctx)
	}
}

// GetDB returns a pointer to the Mongo database.  This is helpful for custom search
// implementations.
func (m *MongoSearcher) GetDB() *mongowrapper.WrappedDatabase {
	return m.db
}

// Search takes a Query and returns a set of results (Resources).
// If an error occurs during the search the corresponding mongo error
// is returned and results will be nil.
func (m *MongoSearcher) Search(query Query) (resources []*models2.Resource, total uint32, err error) {

	// Check to see if we already have a count cached for this query. If so, use it
	// and tell the searcher to skip doing the count. This can only be done reliably if
	// the server is in -readonly mode.
	doCount := true
	var queryHash string

	if m.readonly && m.countTotalResults {
		queryHash = fmt.Sprintf("%x", md5.Sum([]byte(query.Resource+"?"+query.Query)))
		countcacheQuery := bson.D{{Key: "_id", Value: queryHash}}
		countcache := &CountCache{}
		err = m.db.Collection("countcache").FindOne(m.ctx, countcacheQuery).Decode(&countcache)
		if err == nil {
			// Use the cached total and don't bother recomputing it.
			total = countcache.Count
			doCount = false
		}
	}

	// There's no point in running the query if we already know it will return 0 results.
	if m.readonly && !doCount && total == 0 {
		return resources, 0, nil
	}

	// Don't do the count at all if m.countTotalResults is disabled.
	if !m.countTotalResults {
		doCount = false
	}

	var computedTotal uint32
	var cursor *mongo.Cursor
	var start time.Time
	options := query.Options()
	bsonQuery := m.convertToBSON(query) // build the BSON query (without any options)
	usesPipeline := bsonQuery.usesPipeline()

	// Execute the query
	if usesPipeline {
		// The (slower) aggregation pipeline is used if the query contains includes or revincludes

		if glog.V(5) {
			start = time.Now()
			glog.V(5).Infof("aggregate (%s) %#v count=%t", bsonQuery.DebugString(), options, doCount)
		}

		cursor, computedTotal, err = m.aggregate(bsonQuery, options, doCount)

		if glog.V(5) {
			glog.V(5).Infof("   cursor  %+v, total %d, err %+v took %v", cursor, computedTotal, err, time.Since(start))
		}

	} else {
		// Otherwise, the (faster) standard query is used

		if glog.V(5) {
			start = time.Now()
			glog.V(5).Infof("find (%s) %#v count=%t", bsonQuery.DebugString(), options, doCount)
		}
		cursor, computedTotal, err = m.find(bsonQuery, options, doCount)

		if glog.V(5) {
			glog.V(5).Infof("   cursor  %+v, total %d, err %+v took %v", cursor, computedTotal, err, time.Since(start))
		}
	}

	// Check if the query returned any errors
	if err != nil {
		return nil, 0, errors.Wrap(err, "Search error")

		// TODO?
		// if e.Code == opInterruptedCode {
		// 	// This query operation was interrupted
		// 	panic(createOpInterruptedError("Long-running operation interrupted"))
		// }
		// return nil, 0, err
	}

	// If the search was for _summary=count, don't collect the results
	// and just return the total.
	if options.Summary == "count" {
		// results should be an empty slice
		return resources, computedTotal, nil
	}

	// Collect the results
	if cursor != nil {
		for cursor.Next(m.ctx) {
			var document bson.D
			err := cursor.Decode(&document)
			if err != nil {
				return nil, 0, errors.Wrap(err, "Search result decoding error")
			}

			resource, err := models2.NewResourceFromBSON(document)
			if err != nil {
				return nil, 0, errors.Wrap(err, "Search: NewResourceFromBSON failed")
			}
			resources = append(resources, resource)
		}
		if err := cursor.Err(); err != nil {
			return nil, 0, errors.Wrap(err, "Search cursor error")
		}
	}

	// If the count wasn't already in cache, add it to cache.
	if m.readonly && m.countTotalResults && doCount {
		countcache := &CountCache{
			Id:    queryHash,
			Count: computedTotal,
		}
		// Don't collect the error here since this should fail silently.
		m.db.Collection("countcache").InsertOne(m.ctx, countcache)
	}

	// The computed total will only be used if the server had no cached
	// count for this search and countTotalResults is true.
	if doCount {
		total = computedTotal
	}

	return resources, total, nil
}

// aggregate takes a BSONQuery and runs its Pipeline through the mongo aggregation framework. Any query options
// will be added to the end of the pipeline.
func (m *MongoSearcher) aggregate(bsonQuery *BSONQuery, options *QueryOptions, doCount bool) (cursor *mongo.Cursor, total uint32, err error) {
	c := m.db.Collection(models.PluralizeLowerResourceName(bsonQuery.Resource))

	// First get a count of the total results (doesn't apply any options)
	if doCount || options.Summary == "count" {
		if len(bsonQuery.Pipeline) == 1 {
			// The pipeline is only being used for includes/revincludes, meaning the entire
			// collection is being searched. It's faster just to get a total count from the
			// collection after a find operation. The first stage in the Pipeline will
			// always be a $match stage.
			match := bsonQuery.Pipeline[0]["$match"]
			intTotal, err := c.CountDocuments(m.ctx, match)
			if err != nil {
				return nil, 0, err
			}
			total = uint32(intTotal)
		} else {
			// Do the count in the aggregation framework
			countStage := bson.M{"$group": bson.M{
				"_id":   nil,
				"total": bson.M{"$sum": 1},
			}}
			countPipeline := make([]bson.M, len(bsonQuery.Pipeline)+1)
			copy(countPipeline, bsonQuery.Pipeline)
			countPipeline[len(countPipeline)-1] = countStage

			cursor, err := c.Aggregate(m.ctx, countPipeline)
			if err != nil {
				return nil, 0, errors.Wrap(err, "aggregate count failed")
			}
			if cursor.Next(m.ctx) {
				result := struct {
					Total float64 `bson:"total"`
				}{}
				err = cursor.Decode(&result)
				if err != nil {
					return nil, 0, errors.Wrap(err, "aggregate count decode failed")
				}
				if err := cursor.Err(); err != nil {
					return nil, 0, errors.Wrap(err, "aggregate count cursor has an error")
				}
				total = uint32(result.Total)
			} else {
				glog.V(3).Infof("aggregate count --> cursor Next returned false")
				err = cursor.Err()
				if err != nil {
					return nil, 0, errors.Wrap(err, "aggregate count cursor --> next failed")
				}
			}
		}
	}

	if options.Summary == "count" {
		// Just return the count and don't do the search.
		glog.V(3).Infof("returning only total (%d)", total)
		return nil, total, nil
	}

	// Now setup the search pipeline (applying options, if any)
	searchPipeline := bsonQuery.Pipeline
	if options != nil {
		searchPipeline = append(searchPipeline, m.convertOptionsToPipelineStages(bsonQuery.Resource, options)...)
	}
	cursor, err = c.Aggregate(m.ctx, searchPipeline, moptions.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return nil, 0, errors.Wrap(err, "aggregate operation failed")
	}
	glog.V(3).Infof("returning cursor")
	return cursor, total, nil
}

func bson1ArrayToBytes(bson1 []bson.M) []byte {
	bytes, err := bson.Marshal(bson1)
	if err != nil {
		panic(err)
	}
	return bytes
}
func bson1ToBytes(bson1 bson.M) []byte {
	bytes, err := bson.Marshal(bson1)
	if err != nil {
		panic(err)
	}
	return bytes
}

// find takes a BSONQuery and runs a standard mongo search on that query. Any query options are applied
// after the initial search is performed.
func (m *MongoSearcher) find(bsonQuery *BSONQuery, queryOptions *QueryOptions, doCount bool) (cursor *mongo.Cursor, total uint32, err error) {
	c := m.db.Collection(models.PluralizeLowerResourceName(bsonQuery.Resource))

	// First get a count of the total results (doesn't apply any options)
	if doCount || queryOptions.Summary == "count" {
		// c.CountDocuments rather than c.Count works in transactions
		intTotal, err := c.CountDocuments(m.ctx, bsonQuery.Query)
		if err != nil {
			return nil, 0, errors.Wrap(err, "search count operation failed")
		}
		total = uint32(intTotal)
	}

	if queryOptions.Summary == "count" {
		// Just return the count and don't do the search.
		return nil, total, nil
	}

	optionsBundle := moptions.Find()
	if queryOptions != nil {
		removeParallelArraySorts(queryOptions)
		if len(queryOptions.Sort) > 0 {
			fields := bson.D{}
			for i := range queryOptions.Sort {
				// Note: If there are multiple paths, we only look at the first one -- not ideal, but otherwise it gets tricky
				field := convertSearchPathToMongoField(queryOptions.Sort[i].Parameter.Paths[0].Path)
				if queryOptions.Sort[i].Descending {
					fields = append(fields, bson.E{Key: field, Value: -1})
				} else {
					fields = append(fields, bson.E{Key: field, Value: 1})
				}
			}
			optionsBundle = optionsBundle.SetSort(fields)
		}
		if queryOptions.Offset > 0 {
			optionsBundle = optionsBundle.SetSkip(int64(queryOptions.Offset))
		}
		optionsBundle = optionsBundle.SetLimit(int64(queryOptions.Count))
	}

	searchCursor, err := c.Find(m.ctx, bsonQuery.Query, optionsBundle)
	if err != nil {
		return nil, 0, errors.Wrap(err, "search find operation failed")
	}
	return searchCursor, total, nil
}

func (m *MongoSearcher) convertToBSON(query Query) *BSONQuery {
	bsonQuery := NewBSONQuery(query.Resource)

	if query.UsesPipeline() {
		bsonQuery.Pipeline = m.createPipelineObject(query)
	} else {
		bsonQuery.Query = m.createQueryObject(query)
	}
	return bsonQuery
}

func (m *MongoSearcher) createQueryObject(query Query) bson.M {
	return m.createQueryObjectFromParams(query.Params())
}

func (m *MongoSearcher) createQueryObjectFromParams(params []SearchParam) bson.M {
	result := bson.M{}
	for _, p := range m.createParamObjects(params) {
		merge(result, p)
	}
	return result
}

func (m *MongoSearcher) createParamObjects(params []SearchParam) []bson.M {
	results := make([]bson.M, len(params))
	for i, p := range params {
		panicOnUnsupportedFeatures(p)
		switch p := p.(type) {
		case *CompositeParam:
			results[i] = m.createCompositeQueryObject(p)
		case *DateParam:
			results[i] = m.createDateQueryObject(p)
		case *NumberParam:
			results[i] = m.createNumberQueryObject(p)
		case *QuantityParam:
			results[i] = m.createQuantityQueryObject(p)
		case *ReferenceParam:
			results[i] = m.createReferenceQueryObject(p)
		case *StringParam:
			results[i] = m.createStringQueryObject(p)
		case *TokenParam:
			results[i] = m.createTokenQueryObject(p)
		case *URIParam:
			results[i] = m.createURIQueryObject(p)
		case *OrParam:
			results[i] = m.createOrQueryObject(p)
		default:
			// Check for custom search parameter implementations
			builder, err := GlobalMongoRegistry().LookupBSONBuilder(p.getInfo().Type)
			if err != nil {
				panic(createInternalServerError("MSG_PARAM_UNKNOWN", fmt.Sprintf("Parameter \"%s\" not understood", p.getInfo().Name)))
			}
			result, err := builder(p, m)
			if err != nil {
				panic(createInternalServerError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", p.getInfo().Name)))
			}
			results[i] = result
		}
	}

	return results
}

func (m *MongoSearcher) createPipelineObject(query Query) []bson.M {
	standardSearchParams := []SearchParam{}
	chainedSearchParams := []SearchParam{}
	reverseChainedSearchParams := []SearchParam{}

	// Separate out chained and reverse chained search parameters
	for _, p := range query.Params() {
		if usesChainedSearch(p) {
			chainedSearchParams = append(chainedSearchParams, p)
			continue
		}
		if usesReverseChainedSearch(p) {
			reverseChainedSearchParams = append(reverseChainedSearchParams, p)
			continue
		}
		standardSearchParams = append(standardSearchParams, p)
	}

	// Process standard SearchParams
	pipeline := []bson.M{{"$match": m.createQueryObjectFromParams(standardSearchParams)}}

	// Process chained search parameters
	for _, p := range chainedSearchParams {
		pipeline = append(pipeline, m.createChainedSearchPipelineStages(p)...)
	}

	// Process reverse chained search parameters
	for _, p := range reverseChainedSearchParams {
		pipeline = append(pipeline, m.createReverseChainedSearchPipelineStages(p)...)
	}

	return pipeline
}

func (m *MongoSearcher) convertOptionsToPipelineStages(resource string, o *QueryOptions) []bson.M {
	p := []bson.M{}

	// support for _sort
	removeParallelArraySorts(o)
	if len(o.Sort) > 0 {
		var sortBSOND bson.D
		for _, sort := range o.Sort {
			// Note: If there are multiple paths, we only look at the first one -- not ideal, but otherwise it gets tricky
			field := convertSearchPathToMongoField(sort.Parameter.Paths[0].Path)
			order := 1
			if sort.Descending {
				order = -1
			}
			sortBSOND = append(sortBSOND, bson.E{Key: field, Value: order})
		}
		p = append(p, bson.M{"$sort": sortBSOND})
	}

	// support for _offset
	if o.Offset > 0 {
		p = append(p, bson.M{"$skip": o.Offset})
	}
	// support for _count
	p = append(p, bson.M{"$limit": o.Count})

	// support for _include
	if len(o.Include) > 0 {
		for _, incl := range o.Include {
			for _, inclPath := range incl.Parameter.Paths {
				if inclPath.Type != "Reference" {
					continue
				}
				// Mongo paths shouldn't have the array indicators, so remove them
				localField := strings.Replace(inclPath.Path, "[]", "", -1) + ".reference__id"
				for i, inclTarget := range incl.Parameter.Targets {
					if inclTarget == "Any" {
						continue
					}
					from := models.PluralizeLowerResourceName(inclTarget)
					as := fmt.Sprintf("_included%sResourcesReferencedBy%s", inclTarget, strings.Title(incl.Parameter.Name))
					// If there are multiple paths, we need to store each path separately
					if len(incl.Parameter.Paths) > 1 {
						as += fmt.Sprintf("Path%d", i+1)
					}

					p = append(p, bson.M{"$lookup": bson.M{
						"from":         from,
						"localField":   localField,
						"foreignField": "_id",
						"as":           as,
					}})
				}
			}
		}
	}

	// support for _revinclude
	if len(o.RevInclude) > 0 {
		for _, incl := range o.RevInclude {
			// we only want parameters that have the search resource as their target
			targetsSearchResource := false
			for _, inclTarget := range incl.Parameter.Targets {
				if inclTarget == resource || inclTarget == "Any" {
					targetsSearchResource = true
					break
				}
			}
			if !targetsSearchResource {
				continue
			}
			// it comes from the other resource collection
			from := models.PluralizeLowerResourceName(incl.Parameter.Resource)
			// iterate through the paths, adding a join to the pipeline for each one
			for i, inclPath := range incl.Parameter.Paths {
				if inclPath.Type != "Reference" {
					continue
				}
				// Mongo paths shouldn't have the array indicators, so remove them
				foreignField := strings.Replace(inclPath.Path, "[]", "", -1) + ".reference__id"
				as := fmt.Sprintf("_revIncluded%sResourcesReferencing%s", incl.Parameter.Resource, strings.Title(incl.Parameter.Name))
				// If there are multiple paths, we need to store each path separately
				if len(incl.Parameter.Paths) > 1 {
					as += fmt.Sprintf("Path%d", i+1)
				}

				p = append(p, bson.M{"$lookup": bson.M{
					"from":         from,
					"localField":   "_id",
					"foreignField": foreignField,
					"as":           as,
				}})

			}
		}
	}
	return p
}

// The SearchParam argument should be either a ReferenceParam or an OrParam.
func (m *MongoSearcher) createChainedSearchPipelineStages(searchParam SearchParam) []bson.M {
	// This returns stages in the pipeline that represent a chained query reference:
	// 1. One or more $lookup stages for the foreign Resource being referenced (one for each search path)
	// 2. A $match on that foreign Resource

	// Build the $lookups. We need to get a ReferenceParam (of type ChainedQueryReference)
	// that we can use to populate the $lookup. If it's an OR, any one of its Items
	// should do.
	lookupRef, isOr := getLookupReference(searchParam)

	chainedRef, ok := lookupRef.Reference.(ChainedQueryReference)
	if !ok {
		panic(createInternalServerError("", "ReferenceParam is not of type ChainedQueryReference"))
	}

	// We need a $lookup stage for each path, followed by one $match stage
	stages := make([]bson.M, len(lookupRef.getInfo().Paths)+1)
	collectionName := models.PluralizeLowerResourceName(chainedRef.Type)

	for i, path := range lookupRef.Paths {
		stages[i] = bson.M{"$lookup": bson.M{
			"from":         collectionName,
			"localField":   convertSearchPathToMongoField(path.Path) + ".reference__id",
			"foreignField": "_id",
			"as":           "_lookup" + strconv.Itoa(i),
		}}
	}

	// Build the $match. This is based on each ReferenceParam's ChainedQuery, so we'll
	// need to get the SearchParams from those queries first.
	var matchableParams []SearchParam

	if isOr {
		// This gets a little tricky - this is an OR of ReferenceParams, not SearchParams.
		// We need to re-define the OR as an OR of each ReferenceParam's searchable
		// ChainedQuery.Params() results. So let's do that.
		orParam, _ := searchParam.(*OrParam)
		searchableOrParam := buildSearchableOrFromChainedReferenceOr(orParam)
		matchableParams = prependLookupKeyToSearchPaths([]SearchParam{searchableOrParam}, len(lookupRef.Paths))

	} else {
		matchableParams = prependLookupKeyToSearchPaths(chainedRef.ChainedQuery.Params(), len(lookupRef.Paths))
	}

	stages[len(stages)-1] = bson.M{"$match": m.createQueryObjectFromParams(matchableParams)}

	// TODO: Add a $project stage to remove the field after the $match (need Mongo 3.4)
	return stages
}

func (m *MongoSearcher) createReverseChainedSearchPipelineStages(searchParam SearchParam) []bson.M {
	// This returns stages in the pipeline that represent a chained query reference:
	// 1. One or more $lookup stages for the foreign Resource being referenced (one for each search path)
	// 2. A $match on that foreign Resource

	// Build the $lookup. We need to get a ReferenceParam (of type ReverseChainedQueryReference)
	// that we can use to populate the $lookup. If it's an OR, any one of its Items
	// should do.
	lookupRef, isOr := getLookupReference(searchParam)

	revChainedRef, ok := lookupRef.Reference.(ReverseChainedQueryReference)
	if !ok {
		panic(createInternalServerError("", "ReferenceParam is not of type ReverseChainedQueryReference"))
	}

	// We need a $lookup stage for each path, followed by one $match stage
	stages := make([]bson.M, len(lookupRef.getInfo().Paths)+1)
	collectionName := models.PluralizeLowerResourceName(revChainedRef.Type)

	for i, path := range lookupRef.Paths {
		stages[i] = bson.M{"$lookup": bson.M{
			"from":         collectionName,
			"localField":   "_id",
			"foreignField": convertSearchPathToMongoField(path.Path) + ".reference__id",
			"as":           "_lookup" + strconv.Itoa(i),
		}}
	}

	// Build the $match. This is based on each ReferenceParam's Query, so we'll
	// need to get the SearchParams from those queries first.
	var matchableParams []SearchParam

	if isOr {
		// This gets a little tricky - this is an OR of ReferenceParams, not SearchParams.
		// We need to re-define the OR as an OR of each ReferenceParam's searchable
		// Query.Params() results. So let's do that.
		orParam, _ := searchParam.(*OrParam)
		searchableOrParam := buildSearchableOrFromChainedReferenceOr(orParam)
		matchableParams = prependLookupKeyToSearchPaths([]SearchParam{searchableOrParam}, len(lookupRef.Paths))

	} else {
		matchableParams = prependLookupKeyToSearchPaths(revChainedRef.Query.Params(), len(lookupRef.Paths))
	}

	stages[len(stages)-1] = bson.M{"$match": m.createQueryObjectFromParams(matchableParams)}

	// TODO: Add a $project stage to remove the field after the $match (need Mongo 3.4)
	return stages
}

// getLookupReference gets a ReferenceParam needed to do the $lookup stage for a chained
// or reverse chained search in the mongo pipeline. If the reference came from an OrParam,
// isOr is true.
func getLookupReference(searchParam SearchParam) (lookupRef *ReferenceParam, isOr bool) {
	_, isOr = searchParam.(*OrParam)

	if isOr {
		// If it's an OR, any one of its Items should do
		var ok bool
		lookupRef, ok = searchParam.(*OrParam).Items[0].(*ReferenceParam)
		if !ok {
			panic(createInternalServerError("", "Chained search OR has no valid ReferenceParam to use for the $lookup"))
		}
	} else {
		lookupRef = searchParam.(*ReferenceParam)
	}
	return
}

// Prepends "_lookup[i]." to the search path(s), where [i] >= 0. This mutates
// the SearchParams by altering the paths in their SearchParamInfos. To prevent
// modifying the SearchParameterDictionary each SearchParamInfo is cloned before
// being mutated.
func prependLookupKeyToSearchPaths(searchParams []SearchParam, numReferencePaths int) []SearchParam {

	prependStr := "_lookup"

	// Make a copy first so we can safely mutate the params
	matchParams := make([]SearchParam, len(searchParams))
	copy(matchParams, searchParams)

	for _, matchParam := range matchParams {
		switch param := matchParam.(type) {

		case *OrParam:
			// Need to prepend to the OrParam's SearchParam items instead
			for _, item := range param.Items {
				searchInfo := item.getInfo().clone()

				if numReferencePaths > 1 {
					// If we have multiple reference paths we need to duplicate the SearchParamPaths
					// for each matchable SearchParam so we can test against each $lookup in one $or
					// clause.
					duplicatePaths(&searchInfo, numReferencePaths)
				}

				for i, searchPath := range searchInfo.Paths {
					searchInfo.Paths[i].Path = prependStr + strconv.Itoa(i%numReferencePaths) + "." + searchPath.Path
				}
				item.setInfo(searchInfo)
			}
		default:
			searchInfo := matchParam.getInfo().clone()

			if numReferencePaths > 1 {
				duplicatePaths(&searchInfo, numReferencePaths)
			}

			for i, searchPath := range searchInfo.Paths {
				searchInfo.Paths[i].Path = prependStr + strconv.Itoa(i%numReferencePaths) + "." + searchPath.Path
			}
			matchParam.setInfo(searchInfo)
		}
	}
	return matchParams
}

// duplicatePaths duplicates the paths in the SearchParamInfo n times.
// Given paths [a, b] and n = 3, this would return [a, a, a, b, b, b]
func duplicatePaths(info *SearchParamInfo, n int) {

	paths := info.Paths
	numPaths := len(paths)
	newPaths := make([]SearchParamPath, numPaths*n)

	for i := 0; i < numPaths; i++ {
		for j := 0; j < n; j++ {
			newPaths[i*n+j] = paths[i]
		}
	}

	info.Paths = newPaths
}

// Takes an OrParam with two or more ReferenceParam items (of type ChainedQueryReference
// or ReverseChainedQueryReference) and returns an OrParam with two or more SearchParam
// items from those references.
func buildSearchableOrFromChainedReferenceOr(referenceOr *OrParam) *OrParam {
	newOr := &OrParam{
		SearchParamInfo: referenceOr.SearchParamInfo,
	}

	for _, item := range referenceOr.Items {
		refParam, _ := item.(*ReferenceParam)
		var searchParam SearchParam

		switch ref := refParam.Reference.(type) {
		case ChainedQueryReference:
			searchParam = ref.ChainedQuery.Params()[0] // There should only ever be 1 SearchParam here
		case ReverseChainedQueryReference:
			searchParam = ref.Query.Params()[0]
		}
		newOr.Items = append(newOr.Items, searchParam)
	}
	return newOr
}

func panicOnUnsupportedFeatures(p SearchParam) {
	// No prefixes are supported except EQ (the default) and number, date, and quantity prefixes
	_, isDate := p.(*DateParam)
	_, isNumber := p.(*NumberParam)
	_, isQuantity := p.(*QuantityParam)

	prefix := p.getInfo().Prefix
	if prefix != "" && prefix != EQ && !isDate && !isNumber && !isQuantity {
		panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", p.getInfo().Name)))
	}

	// No modifiers are supported except for resource types in reference parameters
	_, isRef := p.(*ReferenceParam)
	modifier := p.getInfo().Modifier
	if modifier != "" {
		if _, ok := SearchParameterDictionary[modifier]; !isRef || !ok {
			panic(createUnsupportedSearchError("MSG_PARAM_MODIFIER_INVALID", fmt.Sprintf("Parameter \"%s\" modifier is invalid", p.getInfo().Name)))
		}
	}
}

func (m *MongoSearcher) createCompositeQueryObject(c *CompositeParam) bson.M {
	panic(createUnsupportedSearchError("MSG_PARAM_UNKNOWN", fmt.Sprintf("Parameter \"%s\" not understood", c.Name)))
}

func (m *MongoSearcher) createDateQueryObject(d *DateParam) bson.M {
	single := func(p SearchParamPath) bson.M {
		switch p.Type {
		case "date", "dateTime":
			return buildBSON(p.Path, dateSelector(d))
		case "instant":
			return buildBSON(p.Path, instantSelector(d))
		case "Period":
			return buildBSON(p.Path, periodSelector(d))
		case "Timing":
			return buildBSON(p.Path+".event", dateSelector(d))
		default:
			return bson.M{}
		}
	}

	return orPaths(single, d.Paths)
}

func dateSelector(d *DateParam) bson.M {
	switch d.Prefix {
	case EQ:
		return bson.M{
			// "the range of the search value fully contains the range of the target value"

			"__from": bson.M{
				"$gte": d.Date.RangeLowIncl(),
			},
			"__to": bson.M{
				"$lte": d.Date.RangeHighExcl(),
			},
		}
	case GT:
		return bson.M{
			// "the range above the search value intersects (i.e. overlaps) with the range of the target value"
			"__to": bson.M{
				"$gt": d.Date.RangeHighExcl(),
			},
		}
	case LT:
		return bson.M{
			// "the range below the search value intersects (i.e. overlaps) with the range of the target value"
			"__from": bson.M{
				"$lt": d.Date.RangeLowIncl(),
			},
		}
	case GE:
		return bson.M{
			"$or": []bson.M{
				bson.M{
					// "the range above the search value intersects (i.e. overlaps) with the range of the target value"
					"__to": bson.M{
						"$gte": d.Date.RangeHighExcl(),
					},
				},
				bson.M{
					// "or the range of the search value fully contains the range of the target value"
					"__from": bson.M{
						"$gte": d.Date.RangeLowIncl(),
					},
				},
			},
		}
	case LE:
		return bson.M{
			"$or": []bson.M{
				bson.M{
					// "the range below the search value intersects (i.e. overlaps) with the range of the target value"
					"__from": bson.M{
						"$lte": d.Date.RangeLowIncl(),
					},
				},
				bson.M{
					// "or the range of the search value fully contains the range of the target value"
					"__to": bson.M{
						"$lte": d.Date.RangeHighExcl(),
					},
				},
			},
		}
	case SA:
		return bson.M{
			// "the range of the search value does not overlap with the range of the target value,
			//  and the range above the search value contains the range of the target value"
			"__from": bson.M{
				"$gt": d.Date.RangeHighExcl(),
			},
		}
	case EB:
		// "the range of the search value does overlap not with the range of the target value,
		//  and the range below the search value contains the range of the target value"
		return bson.M{
			"__to": bson.M{
				"$lt": d.Date.RangeLowIncl(),
			},
		}
	}
	panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", d.Name)))
}

func instantSelector(p *DateParam) bson.M {
	var timestamp bson.M
	switch p.Prefix {
	case EQ:
		timestamp = bson.M{
			"$gte": p.Date.RangeLowIncl(),
			"$lt":  p.Date.RangeHighExcl(),
		}
	case GT:
		timestamp = bson.M{
			"$gt": p.Date.RangeLowIncl(),
		}
	case GE:
		timestamp = bson.M{
			"$gte": p.Date.RangeLowIncl(),
		}
	case SA:
		timestamp = bson.M{
			"$gt": p.Date.RangeHighExcl(),
		}
	case LT, EB:
		timestamp = bson.M{
			"$lt": p.Date.RangeLowIncl(),
		}
	case LE:
		timestamp = bson.M{
			"$lt": p.Date.RangeHighExcl(),
		}
	default:
		panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", p.Name)))
	}

	return timestamp
}

func periodSelector(d *DateParam) bson.M {
	switch d.Prefix {
	case EQ:
		// "the range of the search value fully contains the range of the target value"
		return bson.M{
			"start.__from": bson.M{
				"$gte": d.Date.RangeLowIncl(),
			},
			"end.__to": bson.M{
				"$lte": d.Date.RangeHighExcl(),
			},
		}
	case GT:
		// "the range above the search value intersects (i.e. overlaps) with the range of the target value"
		return bson.M{
			"$or": []bson.M{
				bson.M{
					"end.__to": bson.M{
						"$gt": d.Date.RangeHighExcl(),
					},
				},
				// Also support instances where period exists, but end is null (ongoing)
				bson.M{
					"$ne": nil,
					"end": nil,
				},
			},
		}
	case LT:
		// "the range below the search value intersects (i.e. overlaps) with the range of the target value"
		return bson.M{
			"$or": []bson.M{
				bson.M{
					"start.__from": bson.M{
						"$lt": d.Date.RangeLowIncl(),
					},
				},
				// Also support instances where period exists, but start is null
				bson.M{
					"$ne":   nil,
					"start": nil,
				},
			},
		}
	case GE:
		return bson.M{
			"$or": []bson.M{
				bson.M{
					// "the range above the search value intersects (i.e. overlaps) with the range of the target value"
					"end.__to": bson.M{
						"$gte": d.Date.RangeHighExcl(),
					},
				},
				bson.M{
					// "or the range of the search value fully contains the range of the target value"
					"start.__from": bson.M{
						"$gte": d.Date.RangeLowIncl(),
					},
				},
				// Also support instances where period exists, but end is null (ongoing)
				bson.M{
					"$ne": nil,
					"end": nil,
				},
			},
		}
	case LE:
		return bson.M{
			"$or": []bson.M{
				bson.M{
					// "the range below the search value intersects (i.e. overlaps) with the range of the target value"
					"start.__from": bson.M{
						"$lte": d.Date.RangeLowIncl(),
					},
				},
				bson.M{
					// "or the range of the search value fully contains the range of the target value"
					"end.__to": bson.M{
						"$lte": d.Date.RangeHighExcl(),
					},
				},
				// Also support instances where period exists, but start is null
				bson.M{
					"$ne":   nil,
					"start": nil,
				},
			},
		}
	case SA:
		return bson.M{
			// "the range of the search value does not overlap with the range of the target value,
			//  and the range above the search value contains the range of the target value"
			"start.__from": bson.M{
				"$gt": d.Date.RangeHighExcl(),
			},
		}
	case EB:
		return bson.M{
			// "the range of the search value does overlap not with the range of the target value,
			//  and the range below the search value contains the range of the target value"
			"end.__to": bson.M{
				"$lt": d.Date.RangeLowIncl(),
			},
		}
	}
	panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", d.Name)))
}

func (m *MongoSearcher) createNumberQueryObject(n *NumberParam) bson.M {
	single := func(p SearchParamPath) bson.M {
		l, _ := n.Number.RangeLowIncl().Float64()
		h, _ := n.Number.RangeHighExcl().Float64()
		exact, _ := n.Number.Value.Float64()

		var criteria bson.M

		if p.Type == "decimal" {
			// TODO
			panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" (decimal type) is not yet supported", n.Name)))
		}

		switch n.Prefix {
		case EQ:
			// Equality is in the range [l, h)
			criteria = bson.M{
				"$gte": l,
				"$lt":  h,
			}
		case NE:
			// In the range (-inf, l) || [h, inf)
			criteria = bson.M{
				"$or": []bson.M{
					bson.M{"$lt": l},
					bson.M{"$gte": h},
				},
			}
		case GT:
			criteria = bson.M{
				"$gt": exact,
			}
		case LT:
			criteria = bson.M{
				"$lt": exact,
			}
		case GE:
			criteria = bson.M{
				"$gte": l,
			}
		case LE:
			criteria = bson.M{
				"$lte": h,
			}
		default:
			// SA, EB are not supported for Number queries
			panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", n.Name)))
		}
		return buildBSON(p.Path, criteria)
	}

	return orPaths(single, n.Paths)
}

func (m *MongoSearcher) createQuantityQueryObject(q *QuantityParam) bson.M {
	single := func(p SearchParamPath) bson.M {
		l, _ := q.Number.RangeLowIncl().Float64()
		h, _ := q.Number.RangeHighExcl().Float64()
		exact, _ := q.Number.Value.Float64()

		var criteria bson.M

		switch q.Prefix {
		case EQ:
			criteria = bson.M{
				"value.__from": bson.M{
					"$gte": l,
				},
				"value.__to": bson.M{
					"$lte": h,
				},
			}

		case LT:
			criteria = bson.M{
				"value.__from": bson.M{"$lt": exact},
			}
		case GT:
			criteria = bson.M{
				"value.__to": bson.M{"$gt": exact},
			}
		case GE:
			criteria = bson.M{
				"$or": []bson.M{
					bson.M{
						// "the range above the search value intersects (i.e. overlaps) with the range of the target value"
						"value.__to": bson.M{
							"$gte": h,
						},
					},
					bson.M{
						// "or the range of the search value fully contains the range of the target value"
						"value.__from": bson.M{
							"$gte": l,
						},
					},
				},
			}
		case LE:
			criteria = bson.M{
				"$or": []bson.M{
					bson.M{
						// "the range below the search value intersects (i.e. overlaps) with the range of the target value"
						"value.__from": bson.M{
							"$lte": l,
						},
					},
					bson.M{
						// "or the range of the search value fully contains the range of the target value"
						"value.__to": bson.M{
							"$lte": h,
						},
					},
				},
			}
		default:
			// NE, SA, EB are not supported for Quantity queries
			panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", q.Name)))
		}

		if q.System == "" {

			// FIXME: need to search by both the 'units' and 'code' field...............
			// (http://build.fhir.org/search.html#quantity)
			// however query with $and is not working since the $and seems to need to be at the
			// very top of the mongodb query
			panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\": search by quantity with a code system not yet supported", q.Name)))

			// orClause := []bson.M{
			// 	bson.M{"code": m.ci(q.Code)},
			// 	bson.M{"unit": m.ci(q.Code)},
			// }

			// _, haveExistingOr := criteria["$or"]
			// if haveExistingOr {
			// 	criteria = bson.M{
			// 		"$and": []bson.M{ criteria, bson.M { "$or": orClause } },
			// 	}
			// } else {
			// 	criteria["$or"] = orClause
			// }

		} else {
			criteria["code"] = m.ciToken(q.Code)
			criteria["system"] = m.ciToken(q.System)
		}
		return buildBSON(p.Path, criteria)
	}

	return orPaths(single, q.Paths)
}

func (m *MongoSearcher) createReferenceQueryObject(r *ReferenceParam) bson.M {
	single := func(p SearchParamPath) bson.M {
		if p.Type == "Resource" {
			return m.createInlinedReferenceQueryObject(r, p)
		}
		criteria := bson.M{}
		switch ref := r.Reference.(type) {
		case LocalReference:
			criteria["reference__id"] = ref.ID
			if ref.Type != "" {
				criteria["reference__type"] = ref.Type
			}
		case ExternalReference:
			criteria["reference"] = m.ci(ref.URL)

		case ChainedQueryReference:
			// This should be handled exclusively by the createPipelineObject
			panic(createInternalServerError("", "createReferenceQueryObject should not be used to create ChainedQueryReferences"))

		case ReverseChainedQueryReference:
			// This should be handled exclusively by the createPipelineObject
			panic(createInternalServerError("", "createReferenceQueryObject should not be used to create ReverseChainedQueryReferences"))
		}
		return buildBSON(p.Path, criteria)
	}

	return orPaths(single, r.Paths)
}

func (m *MongoSearcher) createInlinedReferenceQueryObject(r *ReferenceParam, p SearchParamPath) bson.M {
	criteria := bson.M{}
	switch ref := r.Reference.(type) {
	case LocalReference:
		if ref.Type != "" {
			criteria["resourceType"] = ref.Type
		}
		criteria["_id"] = ref.ID
	case ChainedQueryReference:
		criteria = m.createQueryObject(ref.ChainedQuery)
		if ref.Type != "" {
			criteria["resourceType"] = ref.Type
		}
	case ExternalReference:
		panic(createUnsupportedSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", r.Name)))
	}
	return buildBSON(p.Path, criteria)
}

func (m *MongoSearcher) createStringQueryObject(s *StringParam) bson.M {
	single := func(p SearchParamPath) bson.M {
		switch p.Type {
		case "HumanName":
			return buildBSON(p.Path, bson.M{
				"$or": []bson.M{
					bson.M{"text": m.cisw(s.String)},
					bson.M{"family": m.cisw(s.String)},
					bson.M{"given": m.cisw(s.String)},
				},
			})
		case "Address":
			return buildBSON(p.Path, bson.M{
				"$or": []bson.M{
					bson.M{"text": m.cisw(s.String)},
					bson.M{"line": m.cisw(s.String)},
					bson.M{"city": m.cisw(s.String)},
					bson.M{"state": m.cisw(s.String)},
					bson.M{"postalCode": m.cisw(s.String)},
					bson.M{"country": m.cisw(s.String)},
				},
			})
		default:
			if s.Name == "_id" {
				return buildBSON(p.Path, s.String)
			}

			return buildBSON(p.Path, m.ci(s.String))
		}
	}

	return orPaths(single, s.Paths)
}

func (m *MongoSearcher) createTokenQueryObject(t *TokenParam) bson.M {

	var systemCriteria interface{}
	var codeCriteria interface{}
	if t.Code == "" {
		// [parameter]=[system]|
		systemCriteria = m.ciToken(t.System)
	} else if t.System == "" {
		if t.AnySystem {
			// [parameter]=[code]
			codeCriteria = m.ciToken(t.Code)
		} else {
			// [parameter]=|[code]
			codeCriteria = m.ciToken(t.Code)
			systemCriteria = bson.M{"$exists": false}
		}
	} else {
		// [parameter]=[system]|[code]
		codeCriteria = m.ciToken(t.Code)
		systemCriteria = m.ciToken(t.System)
	}

	single := func(p SearchParamPath) bson.M {
		criteria := bson.M{}
		switch p.Type {
		case "Coding":
			if systemCriteria != nil {
				criteria["system"] = systemCriteria
			}
			if codeCriteria != nil {
				criteria["code"] = codeCriteria
			}
		case "CodeableConcept":
			if systemCriteria != nil && codeCriteria != nil {
				criteria["coding"] = bson.M{"$elemMatch": bson.M{"system": systemCriteria, "code": codeCriteria}}
			} else {
				if systemCriteria != nil {
					criteria["coding.system"] = systemCriteria
				}
				if codeCriteria != nil {
					criteria["coding.code"] = codeCriteria
				}
			}

		case "Identifier":
			if systemCriteria != nil {
				criteria["system"] = systemCriteria
			}
			if codeCriteria != nil {
				criteria["value"] = codeCriteria
			}
		case "ContactPoint":
			criteria["value"] = m.ci(t.Code)
			if !t.AnySystem {
				criteria["use"] = m.ciToken(t.System)
			}
		case "boolean":
			switch t.Code {
			case "true":
				return buildBSON(p.Path, true)
			case "false":
				return buildBSON(p.Path, false)
			default:
				panic(createInvalidSearchError("MSG_PARAM_INVALID", fmt.Sprintf("Parameter \"%s\" content is invalid", t.Name)))
			}
		case "string":
			return buildBSON(p.Path, m.ci(t.Code))
		case "code":
			return buildBSON(p.Path, m.ciToken(t.Code))
		case "id":
			// IDs do not need the case-insensitive match.
			return buildBSON(p.Path, t.Code)
		}

		return buildBSON(p.Path, criteria)
	}

	return orPaths(single, t.Paths)
}

func (m *MongoSearcher) createURIQueryObject(u *URIParam) bson.M {
	single := func(p SearchParamPath) bson.M {
		return buildBSON(p.Path, u.URI)
	}

	return orPaths(single, u.Paths)
}

func (m *MongoSearcher) createOrQueryObject(o *OrParam) bson.M {
	return bson.M{
		"$or": m.createParamObjects(o.Items),
	}
}

// Error is an interface for search errors, providing an HTTP status and operation outcome
type Error struct {
	HTTPStatus       int
	OperationOutcome *models.OperationOutcome
}

func (e *Error) Error() string {
	if e.OperationOutcome == nil {
		return fmt.Sprintf("HTTP %d", e.HTTPStatus)
	}
	return fmt.Sprintf("HTTP %d: %s", e.HTTPStatus, e.OperationOutcome.Error())
}

func createUnsupportedSearchError(code, display string) *Error {
	return &Error{
		HTTPStatus:       http.StatusNotImplemented,
		OperationOutcome: models.CreateOpOutcome("error", "not-supported", code, display),
	}
}

func createInvalidSearchError(code, display string) *Error {
	return &Error{
		HTTPStatus:       http.StatusBadRequest,
		OperationOutcome: models.CreateOpOutcome("error", "processing", code, display),
	}
}

func createInternalServerError(code, display string) *Error {
	return &Error{
		HTTPStatus:       http.StatusInternalServerError,
		OperationOutcome: models.CreateOpOutcome("fatal", "exception", code, display),
	}
}

func createOpInterruptedError(display string) *Error {
	return &Error{
		HTTPStatus:       http.StatusInternalServerError,
		OperationOutcome: models.CreateOpOutcome("error", "too-costly", "", display),
	}
}

func buildBSON(path string, criteria interface{}) bson.M {
	result := bson.M{}

	// First fix the indexers so "[0]entry.resource" becomes "entry.0.resource"
	indexedPath := convertBracketIndexesToDotIndexes(path)
	normalizedPath := convertSearchPathToMongoField(path)
	bCriteria, ok := criteria.(bson.M)
	if ok {
		pathRegex := regexp.MustCompile("(.*\\[\\][^\\.]*)\\.?([^\\[\\]]*)")
		if m := pathRegex.FindStringSubmatch(indexedPath); m != nil && len(bCriteria) > 1 {
			// Need to use an $elemMatch because there is an array in the path
			// and the search criteria is a composite
			left := strings.Replace(m[1], "[]", "", -1)
			right := m[2]
			var resultCriteria bson.M
			if len(right) > 0 {
				resultCriteria = bson.M{}
				for k, v := range bCriteria {
					// Pull out the $or and process it separately under $elemMatch
					if isQueryOperator(k) {
						processQueryOperatorCriteria(right, k, v, resultCriteria)
					} else {
						resultCriteria[fmt.Sprintf("%s.%s", right, k)] = v
					}
				}
			} else {
				resultCriteria = bCriteria
			}
			result[left] = bson.M{"$elemMatch": resultCriteria}
		} else {
			// Path has no array or criteria is singular
			for k, v := range bCriteria {
				// Pull out the $or and process it separately as top level condition
				if isQueryOperator(k) {
					processQueryOperatorCriteria(normalizedPath, k, v, result)
				} else {
					result[fmt.Sprintf("%s.%s", normalizedPath, k)] = v
				}
			}
		}
	} else {
		// Criteria is singular, so we don't care about arrays
		result[normalizedPath] = criteria
	}
	return result
}

// Fixes the array markers/indexers so "[]element.[0]target.[]product.element" becomes "element.target.product.element"
func convertSearchPathToMongoField(path string) string {
	indexedPath := convertBracketIndexesToDotIndexes(path)
	return strings.Replace(indexedPath, "[]", "", -1)
}

// Fixes just the indexers so "[]element.[0]target.[]product.element" becomes "element.target.0.product.element"
func convertBracketIndexesToDotIndexes(path string) string {
	re := regexp.MustCompile("\\[(\\d+)\\]([^\\.]+)")
	return re.ReplaceAllString(path, "$2.$1")
}

// MongoDB does not properly sort when keys are in parallel arrays ("Executor error: BadValue cannot sort with keys
// that are parallel arrays"), so... remove any sort options that have parallel arrays (and log it)
func removeParallelArraySorts(o *QueryOptions) {
	npSorts := make([]SortOption, 0, len(o.Sort))
	for i := range o.Sort {
		sort := o.Sort[i]
		isParallel := false
		for _, npSort := range npSorts {
			isParallel = isParallelArrayPath(sort.Parameter.Paths[0].Path, npSort.Parameter.Paths[0].Path)
			if isParallel {
				fmt.Printf("Cannot sub-sort on param '%s' because its path has parallel arrays with previous sort param '%s' (due to limitation in MongoDB)\n.", sort.Parameter.Name, npSort.Parameter.Name)
				break
			}
		}
		if !isParallel {
			npSorts = append(npSorts, sort)
		}
	}
	// If we ended up removing sort options, update the Options object to reflect that
	if len(o.Sort) != len(npSorts) {
		o.Sort = npSorts
	}
}

func isParallelArrayPath(path1 string, path2 string) bool {
	// If one of them doesn't have any arrays then there can't be any parallel arrays
	if !strings.Contains(path1, "[") || !strings.Contains(path2, "[") {
		return false
	}

	// Take out specific indexers for easier comparison (e.g., "[0]key" becomes "[]key")
	re := regexp.MustCompile("\\[\\d+\\]")
	path1 = re.ReplaceAllString(path1, "[]")
	path2 = re.ReplaceAllString(path2, "[]")

	// Now iterate through until non-matching character
	for i := 0; i < len(path1) && i < len(path2); i++ {
		if path1[i] != path2[i] {
			// Check to see if any of the matching part of the path has an array
			return strings.Contains(path1[:i], "[")
		}
	}

	return false
}

func isQueryOperator(key string) bool {
	return len(key) > 0 && key[0] == '$'
}

func processQueryOperatorCriteria(path string, key string, value interface{}, result bson.M) {
	switch key {
	case "$or":
		processOrCriteria(path, value, result)
	default:
		criteria, ok := result[path]
		if !ok {
			criteria = bson.M{}
			result[path] = criteria
		}
		criteria.(bson.M)[key] = value
	}
}

func processOrCriteria(path string, orValue interface{}, result bson.M) {
	if ors, ok := orValue.([]bson.M); ok {
		newOrs := make([]bson.M, len(ors))
		for i := range ors {
			newOrs[i] = buildBSON(path, ors[i])
		}
		result["$or"] = newOrs
	} else {
		panic(createInternalServerError("", ""))
	}
}

// Case-insensitive match
// TODO: consider case-insensitive indexes in MongoDB 3.4 (https://docs.mongodb.com/manual/core/index-case-insensitive/)
func (m *MongoSearcher) ci(s string) interface{} {
	if m.enableCISearches {
		return primitive.Regex{Pattern: fmt.Sprintf("^%s$", regexp.QuoteMeta(s)), Options: "i"}
	}
	return s
}

// Case-insensitive match for token-type search parameters
func (m *MongoSearcher) ciToken(s string) interface{} {

	// R4 leans towards case-sensitive, whereas STU3 text suggests case-insensitive
	// https://github.com/HL7/fhir/commit/13fb1c1f102caf7de7266d6e78ab261efac06a1f

	if !m.tokenParametersCaseSensitive && m.enableCISearches {
		return primitive.Regex{Pattern: fmt.Sprintf("^%s$", regexp.QuoteMeta(s)), Options: "i"}
	}
	return s
}

// Case-insensitive starts-with
// TODO: consider case-insensitive indexes in MongoDB 3.4 (https://docs.mongodb.com/manual/core/index-case-insensitive/)
func (m *MongoSearcher) cisw(s string) interface{} {
	if m.enableCISearches {
		return primitive.Regex{Pattern: fmt.Sprintf("^%s", regexp.QuoteMeta(s)), Options: "i"}
	}
	return s
}

// When multiple paths are present, they should be represented as an OR.
// objFunc is a function that generates a single query for a path
func orPaths(objFunc func(SearchParamPath) bson.M, paths []SearchParamPath) bson.M {
	results := make([]bson.M, 0, len(paths))
	for i := range paths {
		result := objFunc(paths[i])
		// If the bson is just an $or, then bring the components up to the top-level $or
		if len(result) == 1 && result["$or"] != nil {
			nestedOrs := result["$or"].([]bson.M)
			for j := range nestedOrs {
				results = append(results, nestedOrs[j])
			}
		} else {
			results = append(results, objFunc(paths[i]))
		}
	}

	if len(results) == 1 {
		return results[0]
	}

	return bson.M{"$or": results}
}

func merge(into bson.M, from bson.M) {
	var and []bson.M
	if intoAnd, ok := into["$and"]; ok {
		and = intoAnd.([]bson.M)
	}

	for k, v := range from {
		if k == "$and" {
			and = append(and, v.([]bson.M)...)
		} else if _, ok := into[k]; ok {
			and = append(and, bson.M{k: v})
		} else {
			into[k] = v
		}
	}

	if len(and) > 0 {
		into["$and"] = and
	}
}
