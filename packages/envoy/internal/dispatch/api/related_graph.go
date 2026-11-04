package api

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Only issue-owned, addressable nodes participate. In particular a direct message has no
// issue owner and never enters this relation. Each shared target counts once per issue.
const relatedGraphSQL = `with owners(kind,id,issue_key) as not materialized (
 select 'issue',key,key from issues where project_key=$1
 union all select 'artifact',id::text,issue_key from artifacts where project_key=$1 and issue_key is not null
 union all select 'block',b.node_id,a.issue_key from document_blocks b join artifacts a on a.id=b.artifact_id where a.project_key=$1 and a.issue_key is not null
 union all select 'comment',c.id::text,coalesce(c.issue_key,a.issue_key) from comments c left join artifacts a on a.id=c.artifact_id join issues i on i.key=coalesce(c.issue_key,a.issue_key) where i.project_key=$1
 union all select 'message',m.id::text,m.issue_key from messages m join issues i on i.key=m.issue_key where i.project_key=$1
 union all select 'ask',k.id::text,coalesce(k.issue_key,a.issue_key) from asks k left join artifacts a on a.id=k.artifact_id join issues i on i.key=coalesce(k.issue_key,a.issue_key) where i.project_key=$1
 union all select 'comment_block',b.node_id,coalesce(c.issue_key,a.issue_key) from comment_blocks b join comments c on c.id=b.comment_id left join artifacts a on a.id=c.artifact_id join issues i on i.key=coalesce(c.issue_key,a.issue_key) where i.project_key=$1 and b.position is not null
 union all select 'message_block',b.node_id,m.issue_key from message_blocks b join messages m on m.id=b.message_id join issues i on i.key=m.issue_key where i.project_key=$1 and b.position is not null
 union all select 'ask_block',b.node_id,coalesce(k.issue_key,a.issue_key) from ask_blocks b join asks k on k.id=b.ask_id left join artifacts a on a.id=k.artifact_id join issues i on i.key=coalesce(k.issue_key,a.issue_key) where i.project_key=$1 and b.position is not null
), seeds as (
 select * from unnest($2::text[],$3::text[]) as t(kind,id)
 union
 select g.to_kind,g.to_id from ` + refs.AllEdgesSQL + ` g
 join owners o on o.kind=g.from_kind and o.id=g.from_id
 where o.issue_key=$4 and g.kind in ('mentions','affects','child_of','blocked_by','filed_from')
), matched as (
 select o.issue_key, array_agg(distinct g.to_kind||':'||g.to_id order by g.to_kind||':'||g.to_id) as shared
 from ` + refs.AllEdgesSQL + ` g join seeds s on s.kind=g.to_kind and s.id=g.to_id
 join owners o on o.kind=g.from_kind and o.id=g.from_id
 where o.issue_key<>$4 and g.kind in ('mentions','affects','child_of','blocked_by','filed_from')
 group by o.issue_key
)
select i.key,i.title,i.status,m.shared from matched m join issues i on i.key=m.issue_key
where i.project_key=$1
order by (i.status='done'), cardinality(m.shared) desc,i.key limit 3`

func (s *server) graphRelated(ctx context.Context, input relatedInput) ([]model.GraphRelatedIssue, error) {
	kinds, ids := []string{}, []string{}
	for _, ref := range text.Extract(input.text, s.deps.ServerURL) {
		if target, ok := refs.TargetOf(ref); ok {
			kinds = append(kinds, target.Kind)
			ids = append(ids, target.ID)
		}
	}
	source := input.source.issueKey
	if source == "" {
		source = input.excludeIssue
	}
	if len(kinds) == 0 && source == "" {
		return nil, nil
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := s.deps.Store.Pool.Query(bounded, relatedGraphSQL, input.project, kinds, ids, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.GraphRelatedIssue, 0, suggestionRelatedCount)
	for rows.Next() {
		var item model.GraphRelatedIssue
		if err := rows.Scan(&item.Key, &item.Title, &item.Status, &item.Shared); err != nil {
			return nil, err
		}
		item.Href = "/issues/" + item.Key
		items = append(items, item)
	}
	return items, rows.Err()
}
