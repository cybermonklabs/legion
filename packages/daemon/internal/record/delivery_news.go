package record

// DeliveryNews is what one delivery carries of news of the approved design: the items it tells,
// whether it carries the gate's opening to each audience it speaks to, and the start's own text
// the news follows. It travels with the task as encoded JSON (supervise.Delivery.News, opaque to
// supervise as Kept is) and is persisted with the delivery (pending_task_deliveries.design_news).
//
// Three readers take it, each for one thing. The daemon's send (supervise.Deps.DesignNews) reads
// it at every send: it drops each item and opening that no longer stands and renders the text
// again from the record, so the text sent is always Task, a blank line, then the news as the record
// stands at that send. The claim store reads it in SQL, by these JSON names, in the write that
// records the delivery's confirmation, which raises each item's told version and clears each
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

// DeliveryNewsItem is one step whose approved version a delivery tells one audience of one issue:
// its document (Artifact), the version that audience's told row held when the item was composed
// (From, nil where the row did not know it), and the approved version it tells (To). Its
// confirmation raises the row's version of Step to To, and taking that confirmation back puts it
// back to From, or forgets the step where From is nil, which is what the row held.
type DeliveryNewsItem struct {
	Issue    string         `json:"issue"`
	Audience DesignAudience `json:"audience"`
	Step     DesignStep     `json:"step"`
	Artifact string         `json:"artifact"`
	From     *int           `json:"from"`
	To       int            `json:"to"`
}

// Pair is the change set the item names, and whether it names one: from From to To. An item whose
// told row knew no version names none. The reconcile resolves such a version into the row itself
// (Store.FillTold, from Dispatch's review history) before any change set of it is recorded, and a
// start attaches only an item whose change set is recorded, so an item a delivery carries names
// its pair.
func (i DeliveryNewsItem) Pair() (VersionPair, bool) {
	if i.From == nil {
		return VersionPair{}, false
	}
	return VersionPair{ArtifactID: i.Artifact, From: *i.From, To: i.To}, true
}

// DeliveryOpening is one audience of one issue a delivery carries the gate's opening to.
type DeliveryOpening struct {
	Issue    string         `json:"issue"`
	Audience DesignAudience `json:"audience"`
}
