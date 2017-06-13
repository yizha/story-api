/*
   /article/create     GET   [draft (create)]                           no lock
   /article/edit       GET   [version (read) --> draft (create)]        lock on draft
   /article/save       POST  [draft (update)]                           lock on draft
   /article/submit     POST  [draft (save/delete) --> version (create)] lock on draft
   /article/discard    GET   [draft (delete)]                           lock on draft
   /article/publish    GET   [version (read) --> publish (upsert)]      lock on publish
   /article/unpublish  GET   [publish (delete)]                         lock on publish
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	//elastic "gopkg.in/olivere/elastic.v5"
	elastic "github.com/yizha/elastic"
)

const (
	ESScriptSaveArticle = `
if (params.checkuser && ctx._source.locked_by != params.username) {
  ctx.op = "none"
} else {
  ctx._source.headline = params.headline;
  ctx._source.summary = params.summary;
  ctx._source.content = params.content;
  ctx._source.tag = params.tag;
  ctx._source.note = params.note;
}`
)

var (
	draftLock   = &UniqStrMutex{}
	publishLock = &UniqStrMutex{}
)

type JSONTime struct {
	T time.Time
}

func (t *JSONTime) MarshalJSON() ([]byte, error) {
	s := fmt.Sprintf(`"%s"`, t.T.Format("2006-01-02T15:04:05.000Z"))
	return []byte(s), nil
}

func (t *JSONTime) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == `"null"` {
		// set T to the zero value of time.Time
		t.T = time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC", 0))
		return nil
	}
	size := len(s)
	if size != 26 || s[0] != '"' || s[25] != '"' {
		return fmt.Errorf("invalid datetime string: %v", s)
	}
	var err error
	t.T, err = time.Parse("2006-01-02T15:04:05.000Z", s[1:25])
	return err
}

type Article struct {
	Id          string    `json:"id,omitempty"`
	Guid        string    `json:"guid,omitempty"`
	Version     int64     `json:"version,omitempty"`
	Headline    string    `json:"headline,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	Content     string    `json:"content,omitempty"`
	Tag         []string  `json:"tag,omitempty"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   *JSONTime `json:"created_at,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
	RevisedAt   *JSONTime `json:"revised_at,omitempty"`
	RevisedBy   string    `json:"revised_by,omitempty"`
	FromVersion int64     `json:"from_version,omitempty"`
	LockedBy    string    `json:"locked_by,omitempty"`
}

func (a *Article) NilZeroTimeFields() *Article {
	if a.CreatedAt != nil && a.CreatedAt.T.IsZero() {
		a.CreatedAt = nil
	}
	if a.RevisedAt != nil && a.RevisedAt.T.IsZero() {
		a.RevisedAt = nil
	}
	return a
}

func unmarshalArticle(data []byte) (*Article, error) {
	var a Article
	err := json.Unmarshal(data, &a)
	if err != nil {
		return nil, err
	}
	return (&a).NilZeroTimeFields(), nil
}

func getFullArticle(
	client *elastic.Client,
	ctx context.Context,
	index, typ, id string) (*Article, *HttpResponseData) {
	source := elastic.NewFetchSourceContext(true).Include(
		"guid",
		"headline",
		"summary",
		"content",
		"tag",
		"created_at",
		"created_by",
		"revised_at",
		"revised_by",
		"version",
		"from_version",
		"note",
		"locked_by",
	)
	return getArticle(client, ctx, index, typ, id, source)
}

func getArticle(
	client *elastic.Client,
	ctx context.Context,
	index, typ, id string,
	source *elastic.FetchSourceContext) (*Article, *HttpResponseData) {
	getService := client.Get()
	getService.Index(index)
	getService.Type(typ)
	getService.Realtime(false)
	getService.Id(id)
	getService.FetchSourceContext(source)
	resp, err := getService.Do(ctx)
	if err != nil {
		if elastic.IsNotFound(err) {
			body := fmt.Sprintf("article %v not found in index %v type %v!", id, index, typ)
			return nil, CreateNotFoundRespData(body)
		} else {
			body := fmt.Sprintf("failed to query elasticsearch, error: %v", err)
			return nil, CreateInternalServerErrorRespData(body)
		}
	} else if !resp.Found {
		body := fmt.Sprintf("article %v not found in index %v type %v!", id, index, typ)
		return nil, CreateNotFoundRespData(body)
	} else {
		article := &Article{}
		if err := json.Unmarshal(*resp.Source, article); err != nil {
			body := fmt.Sprintf("unmarshal article error: %v", err)
			return nil, CreateInternalServerErrorRespData(body)
		} else {
			article.Id = resp.Id
			return article, nil
		}
	}
}

func marshalArticle(a *Article, status int) *HttpResponseData {
	bytes, err := json.Marshal(a)
	if err != nil {
		body := fmt.Sprintf("error marshaling article: %v", err)
		return CreateInternalServerErrorRespData(body)
	} else {
		return CreateRespData(status, ContentTypeValueJSON, string(bytes))
	}
}

func parseArticleId(id string) (string, int64, error) {
	idx := strings.LastIndex(id, ":")
	if idx <= 0 {
		return id, int64(0), nil
	} else {
		guid := id[0:idx]
		ver, err := strconv.ParseInt(id[idx+1:], 10, 64)
		if err == nil {
			return guid, ver, nil
		} else {
			return guid, int64(0), err
		}
	}
}

func addAuditLogFields(action string, h EndpointHandler) EndpointHandler {
	return func(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
		d := h(app, w, r)
		id := StringFromReq(r, CtxKeyId)
		if id == "" {
			if a, ok := d.Data.(Article); ok {
				id = a.Id
			}
		}
		fields := make(map[string]interface{})
		fields["audit"] = "article"
		fields["action"] = action
		fields["user"] = AuthFromReq(r).Username
		if id != "" {
			guid, ver, err := parseArticleId(id)
			if err == nil {
				fields["article_guid"] = guid
				if ver > 0 {
					fields["article_version"] = ver
				}
			} else {
				CtxLoggerFromReq(r).Perrorf("failed to parse article id %v, error %v", id, err)
			}
		}
		CtxLoggerFromReq(r).AddFields(fields)
		return d
	}
}

func createArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	username := AuthFromReq(r).Username
	article := &Article{
		LockedBy:    username,
		FromVersion: int64(0),
	}
	// don't set Id or OpType in order to have id auto-generated by elasticsearch
	idxService := app.Elastic.Client.Index()
	idxService.Index(app.Conf.ArticleIndex.Name)
	idxService.Type(app.Conf.ArticleIndexTypes.Draft)
	idxService.BodyJson(article)
	resp, err := idxService.Do(context.Background())
	if err != nil {
		body := fmt.Sprintf("error creating new doc: %v", err)
		return CreateInternalServerErrorRespData(body)
	} else {
		article.Id = resp.Id
		article.Guid = resp.Id
		article.CreatedBy = username
		article.LockedBy = username
		if bytes, err := json.Marshal(article); err == nil {
			d := CreateRespData(http.StatusOK, ContentTypeValueJSON, string(bytes))
			// save article so that we can log auto-generated article-id
			// with context-logger
			d.Data = article
			return d
		} else {
			body := fmt.Sprintf("failed to marshal Article object, error: %v", err)
			return CreateInternalServerErrorRespData(body)
		}
	}
}

func saveArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	// don't allow save another user's edit/create
	bytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		body := fmt.Sprintf("failed to read request body, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	}
	article, err := unmarshalArticle(bytes)
	if err != nil {
		body := fmt.Sprintf("failed to unmarshal article, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	}
	id := StringFromReq(r, CtxKeyId)
	username := AuthFromReq(r).Username
	script := elastic.NewScript(ESScriptSaveArticle)
	script.Type("inline").Lang("painless").Params(map[string]interface{}{
		"checkuser":  true,
		"headline":   article.Headline,
		"summary":    article.Summary,
		"content":    article.Content,
		"tag":        article.Tag,
		"note":       article.Note,
		"username":   username,
		"revised_at": &JSONTime{time.Now().UTC()},
	})
	updService := app.Elastic.Client.Update()
	updService.Index(app.Conf.ArticleIndex.Name)
	updService.Type(app.Conf.ArticleIndexTypes.Draft)
	updService.Id(id)
	updService.Script(script)
	updService.DetectNoop(true)

	lock := draftLock.Get(id)
	lock.Lock()
	defer lock.Unlock()
	resp, err := updService.Do(context.Background())
	//fmt.Printf("resp: %T, %+v\n", resp, resp)
	//fmt.Printf("error: %v\n", err)
	if err != nil {
		if elastic.IsNotFound(err) {
			body := fmt.Sprintf("article %v not found!", id)
			return CreateNotFoundRespData(body)
		} else {
			body := fmt.Sprintf("failed to update article, error: %v", err)
			return CreateInternalServerErrorRespData(body)
		}
	} else {
		if resp.Result == "noop" {
			return CreateForbiddenRespData("Update article locked by another user is not allowed!")
		} else if resp.Result == "updated" {
			return CreateRespData(http.StatusOK, ContentTypeValueText, "")
		} else {
			body := fmt.Sprintf(`unknown "result" in update response: %v`, resp.Result)
			return CreateInternalServerErrorRespData(body)
		}
	}
}

func submitArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	// first save the article to draft
	// here we allow 'super' user to submit
	// article created/edited by a different user
	bytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		body := fmt.Sprintf("failed to read request body, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	}
	article, err := unmarshalArticle(bytes)
	if err != nil {
		body := fmt.Sprintf("failed to unmarshal article, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	}
	guid := StringFromReq(r, CtxKeyId)
	username := AuthFromReq(r).Username
	ts := time.Now().UTC()
	jt := &JSONTime{ts}
	script := elastic.NewScript(ESScriptSaveArticle)
	script.Type("inline").Lang("painless").Params(map[string]interface{}{
		"checkuser":  false,
		"headline":   article.Headline,
		"summary":    article.Summary,
		"content":    article.Content,
		"tag":        article.Tag,
		"note":       article.Note,
		"username":   username,
		"revised_at": jt,
	})
	client := app.Elastic.Client
	updService := client.Update()
	updService.Index(app.Conf.ArticleIndex.Name)
	updService.Type(app.Conf.ArticleIndexTypes.Draft)
	updService.Id(guid)
	updService.Script(script)
	updService.DetectNoop(false)
	ctx := context.Background()

	lock := draftLock.Get(guid)
	lock.Lock()
	defer lock.Unlock()
	_, err = updService.Do(ctx)
	if err != nil {
		body := fmt.Sprintf("failed to save article draft %v, error: %v", guid, err)
		return CreateInternalServerErrorRespData(body)
	}
	// set article props for the new version
	ver := ts.UnixNano()
	verGuid := fmt.Sprintf("%v:%v", guid, ver)
	article.Id = verGuid
	article.Version = ver
	if article.FromVersion == 0 { // it's a create
		article.CreatedAt = jt
		article.CreatedBy = username
		article.RevisedAt = nil
		article.RevisedBy = ""
	} else { // it's an edit
		article.RevisedAt = jt
		article.RevisedBy = username
	}
	article.LockedBy = ""
	// create the new version
	idxService := client.Index()
	idxService.Index(app.Conf.ArticleIndex.Name)
	idxService.Type(app.Conf.ArticleIndexTypes.Version)
	idxService.OpType(ESIndexOpCreate)
	idxService.Id(article.Id)
	idxService.BodyJson(article)
	idxResp, err := idxService.Do(ctx)
	if err != nil {
		body := fmt.Sprintf("failed to create new article version, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	} else if !idxResp.Created {
		body := "no reason but article new version is not created!"
		return CreateInternalServerErrorRespData(body)
	}
	// delete article from draft
	delService := client.Delete()
	delService.Index(app.Conf.ArticleIndex.Name)
	delService.Type(app.Conf.ArticleIndexTypes.Draft)
	delService.Id(article.Guid)
	_, err = delService.Do(ctx)
	if err != nil {
		body := fmt.Sprintf("failed to delete article draft, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	}
	// return article version
	article.Headline = ""
	article.Summary = ""
	article.Content = ""
	article.Tag = nil
	article.Note = ""
	return marshalArticle(article, http.StatusOK)
}

func discardArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	id := StringFromReq(r, CtxKeyId)
	delService := app.Elastic.Client.Delete()
	delService.Index(app.Conf.ArticleIndex.Name)
	delService.Type(app.Conf.ArticleIndexTypes.Draft)
	delService.Id(id)

	lock := draftLock.Get(id)
	lock.Lock()
	defer lock.Unlock()
	_, err := delService.Do(context.Background())
	if err != nil {
		if elastic.IsNotFound(err) {
			body := fmt.Sprintf("article %v not found!", id)
			return CreateNotFoundRespData(body)
		} else {
			body := fmt.Sprintf("failed to discard article, error: %v", err)
			return CreateInternalServerErrorRespData(body)
		}
	} else {
		return CreateRespData(http.StatusOK, ContentTypeValueText, "")
	}
}

func editArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	// first get the article from the version type
	id := StringFromReq(r, CtxKeyId)
	client := app.Elastic.Client
	index := app.Conf.ArticleIndex.Name
	typ := app.Conf.ArticleIndexTypes.Version
	ctx := context.Background()
	article, d := getFullArticle(client, ctx, index, typ, id)
	if d != nil {
		return d
	}
	// set article props
	user := AuthFromReq(r).Username
	article.FromVersion = article.Version
	article.Version = 0
	article.RevisedAt = nil
	article.RevisedBy = user
	article.LockedBy = user
	// now try to index (create) it as type draft
	typ = app.Conf.ArticleIndexTypes.Draft
	idxService := client.Index()
	idxService.Index(index)
	idxService.Type(typ)
	idxService.OpType(ESIndexOpCreate)
	idxService.Id(article.Guid)
	idxService.BodyJson(article)

	lock := draftLock.Get(article.Guid)
	lock.Lock()
	defer lock.Unlock()
	resp, err := idxService.Do(ctx)
	if err != nil {
		body := fmt.Sprintf("error querying elasticsearch, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	} else if !resp.Created {
		// same doc already there? try to load it
		source := elastic.NewFetchSourceContext(true).Include("guid", "version", "from_version", "locked_by")
		article, d = getArticle(client, ctx, index, typ, article.Guid, source)
		if d != nil {
			// edge case: affending resource not found (404),
			// return 409 so that caller could give it another try
			if d.Status == http.StatusNotFound {
				d = marshalArticle(&Article{
					Guid: article.Guid,
				}, http.StatusConflict)
			}
			return d
		} else {
			return marshalArticle(article, http.StatusConflict)
		}
	} else {
		return marshalArticle(article, http.StatusOK)
	}
}

func publishArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	// load article from version
	client := app.Elastic.Client
	ctx := context.Background()
	index := app.Conf.ArticleIndex.Name
	typ := app.Conf.ArticleIndexTypes.Version
	id := StringFromReq(r, CtxKeyId)
	article, d := getFullArticle(client, ctx, index, typ, id)
	if d != nil {
		return d
	}
	// upsert it into publish
	guid := article.Guid
	article.Id = guid
	article.LockedBy = ""
	updService := client.Update()
	updService.Index(index)
	updService.Type(app.Conf.ArticleIndexTypes.Publish)
	updService.Id(guid)
	updService.Doc(article)
	updService.DocAsUpsert(true)

	lock := publishLock.Get(guid)
	lock.Lock()
	defer lock.Unlock()
	_, err := updService.Do(ctx)
	if err != nil {
		body := fmt.Sprintf("failed to publish article, error: %v", err)
		return CreateInternalServerErrorRespData(body)
	} else {
		return CreateRespData(http.StatusOK, ContentTypeValueText, "")
	}
}

func unpublishArticle(app *AppRuntime, w http.ResponseWriter, r *http.Request) *HttpResponseData {
	id := StringFromReq(r, CtxKeyId)
	delService := app.Elastic.Client.Delete()
	delService.Index(app.Conf.ArticleIndex.Name)
	delService.Type(app.Conf.ArticleIndexTypes.Publish)
	delService.Id(id)

	lock := publishLock.Get(id)
	lock.Lock()
	defer lock.Unlock()
	_, err := delService.Do(context.Background())
	if err != nil {
		if elastic.IsNotFound(err) {
			body := fmt.Sprintf("article %v not found!", id)
			return CreateNotFoundRespData(body)
		} else {
			body := fmt.Sprintf("failed to unpublish article, error: %v", err)
			return CreateInternalServerErrorRespData(body)
		}
	} else {
		return CreateRespData(http.StatusOK, ContentTypeValueText, "")
	}
}

func ArticleCreate(app *AppRuntime) EndpointHandler {
	return addAuditLogFields("create", createArticle)
}

func ArticleSave(app *AppRuntime) EndpointHandler {
	h := addAuditLogFields("save", saveArticle)
	return GetRequiredStringArg("id", CtxKeyId, h)
}

func ArticleSubmit(app *AppRuntime) EndpointHandler {
	h := addAuditLogFields("submit", submitArticle)
	return GetRequiredStringArg("id", CtxKeyId, h)
}

func ArticleDiscard(app *AppRuntime) EndpointHandler {
	h := addAuditLogFields("discard", discardArticle)
	return GetRequiredStringArg("id", CtxKeyId, h)
}

func ArticleEdit(app *AppRuntime) EndpointHandler {
	h := addAuditLogFields("edit", editArticle)
	return GetRequiredStringArg("id", CtxKeyId, h)
}

func ArticlePublish(app *AppRuntime) EndpointHandler {
	h := addAuditLogFields("publish", publishArticle)
	return GetRequiredStringArg("id", CtxKeyId, h)
}

func ArticleUnpublish(app *AppRuntime) EndpointHandler {
	h := addAuditLogFields("unpublish", unpublishArticle)
	return GetRequiredStringArg("id", CtxKeyId, h)
}
