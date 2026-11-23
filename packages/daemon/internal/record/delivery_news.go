package record

import "time"

// DeliveryNews is what one delivery carries of news of the approved design: the items it tells,
// the gate's opening it carries to each audience it speaks to, and the start's own text the news
// follows. It travels with the task as encoded JSON (supervise.Delivery.News, opaque to supervise
// as Kept is) and is persisted with the delivery (pending_task_deliveries.design_news).
//
// Three readers take it, each for one thing. The daemon's send (supervise.Deps.DesignNews) reads
// it at every send: it drops each item and opening that no longer stands, re-keys each item it
// carries to the version its told row holds now, and renders the text again from the record, so
// the text sent is always Task, a blank line, then the news as the record stands at that send, and
// the news the delivery carries is exactly what that text names, each item and opening marked with
// the run it was rendered in (DeliveryNewsItem.Generation and TreeGeneration). The claim store
// reads it in SQL, by these JSON names, from the copy the prompt a confirmed turn answers carried
// (supervise.Delivery.Carried, persisted as pending_task_deliveries.carried_news): while the
// delivery holds that confirmation, the copy is the delivery's credit, which every read of what an
// audience took counts (the SQL function design_news_told_in, Postgres.TreeTold), and once the
// turn's result says the turn ran, the write that records it folds the copy into the told rows
// (store's settleDesignNews). A take-back or a retirement of the delivery ends its credit with its
// row and writes nothing else. So the prompt that carried an item is the one thing that says the
// audience took it, and a delivery retired unconfirmed tells nothing.
type DeliveryNews struct {
	// Task is the start's own text, before any news: the task the start would carry had nothing
	// been owed. The send renders the news after it, so a phase start whose every item stopped
	// standing is sent as this text alone. A news-only start may carry none, and is then its news
	// alone. Whether the start was queued for the news alone is the delivery's own mark
	// (supervise.Delivery.NewsOnly, pending_task_deliveries.news_only), never this news's.
	Task string `json:"task"`
	// Items is each step's approved version the delivery tells, each to one audience of one issue.
	Items []DeliveryNewsItem `json:"items,omitempty"`
	// Openings is each audience the delivery says the gate's state to in place of the opening's
	// role message: one whose told row owed the opening when the delivery took it on
	// (DesignTold.OpeningOwed), so its news leads with the gate sentence.
	Openings []DeliveryOpening `json:"openings,omitempty"`
}

// Empty says whether the news tells nothing: no item and no opening. A delivery carries no such news.
func (n DeliveryNews) Empty() bool {
	return len(n.Items) == 0 && len(n.Openings) == 0
}

// DeliveryNewsItem is one step whose approved version a delivery tells one audience of one issue:
// its document (Artifact), the version that audience's told row held when the item was last
// rendered (From, nil where the row did not know it), the approved version it tells (To), the run
// of its issue at that render (Generation and TreeGeneration), and, for an item whose row knew no
// version, the review history that could not say which, as the start attached it
// (HistoryUnread). Its credit is the pair: it reaches a told row that took From or more (any row,
// for a From of nil), and takes the step to To (the SQL function design_news_known), so a credit
// counted from a version another credit gave reaches nothing once that credit is given back.
type DeliveryNewsItem struct {
	Issue    string         `json:"issue"`
	Audience DesignAudience `json:"audience"`
	Step     DesignStep     `json:"step"`
	Artifact string         `json:"artifact"`
	From     *int           `json:"from"`
	To       int            `json:"to"`
	// Generation is the generation of Issue, and TreeGeneration the generation of the root of the
	// tree Issue stood in, when the item was rendered (Issue.Generation of each): the run the news
	// was told to. A clear of a generation (Postgres.ClearGeneration, ClearTreeGeneration) deletes
	// what the holders of each issue it covers took, and the write beside it raises one of the two:
	// a child set back to todo (workflow's ReenterChild) and an issue moved into another tree
	// (workflow's moveIntoTree) raise the issue's own, and a root set back to todo (admit's
	// readmit) raises the root's, which every member of its tree reads as its tree's. So an item
	// counts as a credit, and folds into a told row, only while both still equal the record's (the
	// SQL function design_news_current): news told to a run that is over tells the next run
	// nothing, and stays owed to it.
	Generation     uint64 `json:"generation"`
	TreeGeneration uint64 `json:"tree_generation"`
	// HistoryUnread is the unread review history the item was attached with (DesignHistoryUnread):
	// a start attaches an item whose told row knows no version only while the record holds that
	// history unread and no reconcile of its tree is pending, and attaches it with this. Every later
	// send of the delivery tells the item by it, whatever the record has done since, until the told
	// row knows a version whose change set is recorded, which the send tells instead; so a re-send
	// never loses what the start attached. Nil for an item that names its pair, and for an item of a
	// delivery written before the field was, which the send reads as a start reads the record (the
	// daemon's readNewsAtSend).
	HistoryUnread *DeliveryHistoryUnread `json:"history_unread,omitempty"`
}

// DeliveryHistoryUnread is what a delivery carries of an unread review history
// (DeliveryNewsItem.HistoryUnread): why the read failed, and when the reconcile gave up on it.
type DeliveryHistoryUnread struct {
	Reason string    `json:"reason"`
	ReadAt time.Time `json:"read_at"`
}

// NewsItem is the item as the record owes it (NewsItem): its step, its document and its two
// versions, without the audience it is told to.
func (i DeliveryNewsItem) NewsItem() NewsItem {
	return NewsItem{Step: i.Step, ArtifactID: i.Artifact, From: i.From, To: i.To}
}

// Unread is the unread review history the item carries, as the record names it
// (DesignHistoryUnread), nil for an item that carries none.
func (i DeliveryNewsItem) Unread() *DesignHistoryUnread {
	if i.HistoryUnread == nil {
		return nil
	}
	return &DesignHistoryUnread{DesignHistoryQuestion: DesignHistoryQuestion{ArtifactID: i.Artifact, To: i.To},
		Reason: i.HistoryUnread.Reason, ReadAt: i.HistoryUnread.ReadAt}
}

// DeliveryOpening is one audience of one issue a delivery says the gate's state to: the opening its
// told row owed when the send rendered it, named by the number the row held for it then (Seq,
// DesignTold.Opening) and the run of its issue then (Generation and TreeGeneration, as an item's),
// or, where the gate closed again after the delivery took that opening on, that the gate is closed
// (Closed). The credit of a confirmed delivery that carried it says the opening told only where it
// said opened, its issue still stands in that run, and the row still holds that number, and so
// does the fold of that credit once its turn's result says it ran (the claim store's
// settleDesignNews, through the SQL function design_news_opening_told, in which a missing number
// matches no row). A close, a reopening and the opened role message each give the row its next
// number (Postgres.SettleOpening, CloseOpenings, through design_news_opening_settled), so no credit
// of an earlier opening tells a later one, and no give-back owes again an opening that something
// else has told or ended since. A clear writes the next run's row from number 0 again, so the run
// is what keeps an opening the last run carried from telling one of the next run's.
type DeliveryOpening struct {
	Issue          string         `json:"issue"`
	Audience       DesignAudience `json:"audience"`
	Seq            int            `json:"seq"`
	Closed         bool           `json:"closed,omitempty"`
	Generation     uint64         `json:"generation"`
	TreeGeneration uint64         `json:"tree_generation"`
}
