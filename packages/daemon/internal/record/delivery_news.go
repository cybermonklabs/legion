package record

import "time"

// DeliveryNews is what one delivery carries of news of the approved design: the items it tells,
// whether it carries the gate's opening to each audience it speaks to, and the start's own text
// the news follows. It travels with the task as encoded JSON (supervise.Delivery.News, opaque to
// supervise as Kept is) and is persisted with the delivery (pending_task_deliveries.design_news).
//
// Three readers take it, each for one thing. The daemon's send (supervise.Deps.DesignNews) reads
// it at every send: it drops each item and opening that no longer stands, re-keys an item it
// carried with no earlier version to the version its told row has come to know, and renders the
// text again from the record, so the text sent is always Task, a blank line, then the news as the
// record stands at that send. The claim store reads it in SQL, by these JSON names, in the write
// that records the delivery's confirmation, which raises each item's told version and clears each
// opening it carried (store's tellDesignNews), and in the write that takes that confirmation back,
// which puts both back (store's untellDesignNews). So the delivery that carried an item is the one
// thing that says the audience took it, and a delivery retired unconfirmed tells nothing.
type DeliveryNews struct {
	// Task is the start's own text, before any news: the task the start would carry had nothing
	// been owed. The send renders the news after it, so a phase start whose every item stopped
	// standing is sent as this text alone. A news-only start may carry none, and is then its news
	// alone.
	Task string `json:"task"`
	// NewsOnly says the start was queued for the news alone (record.SuperviseRequest.News): left
	// with nothing to say at its send, it is retired rather than sent.
	NewsOnly bool `json:"news_only,omitempty"`
	// Items is each step's approved version the delivery tells, each to one audience of one issue.
	Items []DeliveryNewsItem `json:"items,omitempty"`
	// Openings is each audience the delivery carries the gate's opening to: one whose told row
	// says the opening is owed (DesignTold.OpeningOwed), so its news leads with the gate sentence
	// in place of the opening's role message.
	Openings []DeliveryOpening `json:"openings,omitempty"`
}

// Empty says whether the news tells nothing: no item and no opening. A delivery carries no such news.
func (n DeliveryNews) Empty() bool {
	return len(n.Items) == 0 && len(n.Openings) == 0
}

// DeliveryNewsItem is one step whose approved version a delivery tells one audience of one issue:
// its document (Artifact), the version that audience's told row held when the item was last
// rendered (From, nil where the row did not know it), the approved version it tells (To), and,
// for an item whose row knew no version, the review history that could not say which, as the start
// attached it (HistoryUnread). Its confirmation raises the row's version of Step to To, whatever
// From is, and taking that confirmation back puts it back to From, or forgets the step where From
// is nil, which is what the row held.
type DeliveryNewsItem struct {
	Issue    string         `json:"issue"`
	Audience DesignAudience `json:"audience"`
	Step     DesignStep     `json:"step"`
	Artifact string         `json:"artifact"`
	From     *int           `json:"from"`
	To       int            `json:"to"`
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

// DeliveryOpening is one audience of one issue a delivery carries the gate's opening to.
type DeliveryOpening struct {
	Issue    string         `json:"issue"`
	Audience DesignAudience `json:"audience"`
}
