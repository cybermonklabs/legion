package api

import (
 "context"
 "encoding/json"
 "net/http"
 "net/url"
 "slices"
 "testing"

 "github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestIssueLinksGraphDerivesDuplicateAndPreservesLegacyLinks(t *testing.T) {
 handler, database, _ := newTestServer(t, testServerOptions{})
 createSuggestionProject(t, handler, "EDGE")
 parent := createSearchIssue(t, handler, "EDGE", "Parent topic", "")
 target := createSearchIssue(t, handler, "EDGE", "Canonical topic", "")
 source := createSuggestionIssue(t, handler, map[string]any{"project":"EDGE", "title":"Repeated topic", "parent":parent, "blocked_by":[]string{target}})
 createIssueMessage(t, handler, source, map[string]any{"body":"Duplicate of dispatch://"+target+"."}, "alice")
 response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+source, map[string]any{"status":"done"}, "alice")
 if response.Code != http.StatusOK { t.Fatalf("close: %d %s", response.Code, response.Body.String()) }
 response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source+"/links", nil, "alice")
 if response.Code != http.StatusOK { t.Fatalf("links: %d %s", response.Code, response.Body.String()) }
 links := decodeBody[model.GraphReferences](t,response)
 kinds := []string{}
 for _, edge := range links.Edges { kinds=append(kinds,edge.Kind); if edge.Kind=="duplicate_of" && edge.Node.ID!=target { t.Fatalf("wrong duplicate: %+v",edge) } }
 for _, kind := range []string{"child_of","blocked_by","duplicate_of"} { if !slices.Contains(kinds,kind) { t.Fatalf("missing %s in %+v",kind,links) } }
 response=dispatchRequest(t,handler,http.MethodGet,"/api/v1/issues/"+source+"/links/shadow",nil,"alice")
 if response.Code!=http.StatusOK { t.Fatalf("shadow: %d %s",response.Code,response.Body.String()) }
 var shadow struct { Mismatches int `json:"mismatches"`; Legacy int `json:"legacy"` }
 if err:=json.Unmarshal(response.Body.Bytes(),&shadow);err!=nil {t.Fatal(err)}
 if shadow.Mismatches!=0 || shadow.Legacy!=2 { t.Fatalf("shadow=%s",response.Body.String()) }
 // Negative control: remove a legacy edge from the view only; source records stay intact.
 if _,err:=database.Pool.Exec(context.Background(),`create or replace view graph_edges as select from_kind,from_id,kind,to_kind,to_id,created_at,source_seq from refs`);err!=nil {t.Fatal(err)}
 response=dispatchRequest(t,handler,http.MethodGet,"/api/v1/issues/"+source+"/links/shadow",nil,"alice")
 if err:=json.Unmarshal(response.Body.Bytes(),&shadow);err!=nil {t.Fatal(err)}
 if shadow.Mismatches!=2 {t.Fatalf("negative control=%s",response.Body.String())}
}

func TestIssueFiledFromIsExplicitAndRejectsPrivateOrWrongScopeSources(t *testing.T) {
 handler, _, _ := newTestServer(t,testServerOptions{})
 createSuggestionProject(t,handler,"FROM")
 source:=createSearchIssue(t,handler,"FROM","Source discussion","")
 message:=createIssueMessage(t,handler,source,map[string]any{"body":"Please file the parser defect."},"alice")
 ref:="dispatch://"+source+"/message/"+message.ID
 created:=createSuggestionIssue(t,handler,map[string]any{"project":"FROM","title":"Parser defect","filed_from":ref})
 response:=dispatchRequest(t,handler,http.MethodGet,"/api/v1/issues/"+created+"/links",nil,"alice")
 if response.Code!=http.StatusOK {t.Fatalf("links: %d %s",response.Code,response.Body.String())}
 links:=decodeBody[model.GraphReferences](t,response)
 if len(links.Edges)!=1 || links.Edges[0].Kind!="filed_from" || links.Edges[0].Node.Ref!=ref {t.Fatalf("links=%+v",links)}
 for _,bad:=range []string{"dispatch://agent/private/message/"+message.ID,"dispatch://"+source+"/message/00000000-0000-4000-8000-000000000000", "dispatch://"+source} {
 response=dispatchRequest(t,handler,http.MethodPost,"/api/v1/issues",map[string]any{"project":"FROM","title":"Another distinct defect","filed_from":bad},"alice")
 if response.Code!=http.StatusBadRequest {t.Fatalf("filed_from %s: %d %s",bad,response.Code,response.Body.String())}
 }
}

func TestRelatedGraphFindsSharedSpecReferenceWhenTextMisses(t *testing.T) {
 handler,database,_:=newTestServer(t,testServerOptions{})
 createSuggestionProject(t,handler,"INFRA")
 foundation:=createSearchIssue(t,handler,"INFRA","Foundational substrate","")
 // Shapes from the opened INFRA-1044 and INFRA-1098 specs: both cite INFRA-156,
 // but the original asks use different words. No folded-into citation is seeded.
 target:=createSearchIssue(t,handler,"INFRA","Personal correspondence overview","Builds on dispatch://"+foundation)
 createSearchIssue(t,handler,"INFRA","Ceramic kiln maintenance","")
 refreshTermStats(t,database,"INFRA")
 params:=url.Values{"title":{"Tuple relationships unify notification channels"},"text":{"Inspired by dispatch://"+foundation},"keywords_only":{"true"}}
 response:=dispatchRequest(t,handler,http.MethodGet,"/api/v1/projects/INFRA/related?"+params.Encode(),nil,"alice")
 if response.Code!=http.StatusOK {t.Fatalf("related: %d %s",response.Code,response.Body.String())}
 var result struct { Related []model.WriteSuggestion `json:"related"`; Graph []struct { Key string `json:"key"`; Shared []string `json:"shared"` } `json:"graph_related"` }
 if err:=json.Unmarshal(response.Body.Bytes(),&result);err!=nil {t.Fatal(err)}
 for _,item:=range result.Related {if item.Owner.Key==target {t.Fatalf("text unexpectedly found target: %s",response.Body.String())}}
 if len(result.Graph)==0 || result.Graph[0].Key!=target {t.Fatalf("graph missed %s: %s",target,response.Body.String())}
 // Removing the shared citation removes the signal, not just its explanation.
 params.Set("text","No cited foundation")
 response=dispatchRequest(t,handler,http.MethodGet,"/api/v1/projects/INFRA/related?"+params.Encode(),nil,"alice")
 if err:=json.Unmarshal(response.Body.Bytes(),&result);err!=nil {t.Fatal(err)}
 if len(result.Graph)!=0 {t.Fatalf("negative control=%s",response.Body.String())}
}
