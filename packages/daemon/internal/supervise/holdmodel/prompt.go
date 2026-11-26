package holdmodel

func invalidate(r *Row) {
	for k, a := range r.Att {
		if a.open() {
			r.Att[k].Stage = AtInvalid
		}
		r.Att[k].TurnBy = 0
	}
	r.TurnBy, r.RecoverBy = 0, 0
}

func refreshTurnBy(r *Row) {
	r.TurnBy = 0
	for _, a := range r.Att {
		if a.open() {
			r.TurnBy = earlier(r.TurnBy, a.TurnBy)
		}
	}
}
func take(r *Row) {
	if r.Task.L != 0 {
		r.Task.ID++
		r.Task.Conf = 0
		r.Task.Interrupted = true
		r.Task.Unknown = false
	}
}
func retire(r *Row, why, k uint8) {
	l := r.Task.L
	if l != 0 {
		r.Ret[l] = why
		if why == RServed {
			r.ServedBy[l] = k + 1
		}
		invalidate(r)
		r.Task = Task{}
	}
}

// queue is D5: a newer task waits behind recovery doubt, a legacy row's task
// whose attempts are unknown (D12 condition 2), and a confirmed task with an
// answer to come.
func (d *decision) queue(e Event) {
	r := d.M
	if r.Task.L == e.A || r.Ret[e.A] != 0 || r.Task.L != 0 && (r.RecoverBy != 0 || r.Task.Unknown || r.Task.Conf != 0 && r.waiting() || e.K == KSupersede && r.outstanding()) {
		return
	}
	retire(&r, RReplaced, 0)
	r.Task = Task{L: e.A, ID: 1, PhaseTask: e.B == 1}
	r.Meta = e.A
	d.Prepared = 0
	d.write(DeliveryWrite, r, e)
}
func (d *decision) prepare() { d.W.Alloc++; d.Prepared = d.W.Alloc; d.Resend = false }
func (d *decision) register(e Event) {
	r := d.M
	r.Att[d.Prepared-1] = Attempt{Stage: AtReg, L: r.Task.L, Under: r.Task.ID, News: r.Meta}
	d.write(RegisterWrite, r, e)
}
func (d *decision) wire(k uint8) {
	w := &d.W
	r := d.M
	a := r.Att[k]
	q := WireRequest{L: a.L, Under: a.Under, Conn: w.Conn, Inc: w.Inc, Root: k, Written: true}
	d.add(Send, 0, k)
	d.H.Requests[q.L]++
	if d.H.Recovering&(1<<q.L) != 0 {
		d.H.Bad |= Twice // D10 forbids even a deduplicated second request here
	}
	if w.Dedupe != 0 {
		prev := w.Requests[w.Dedupe-1]
		if prev.L == q.L && prev.Under == q.Under && prev.Inc == q.Inc {
			q.Root = w.Dedupe - 1
			w.Requests[k] = q
			d.add(Deduplicate, 0, k)
			return
		}
	}
	w.Dedupe = k + 1
	w.Requests[k] = q
	d.add(Forward, 0, k)
	h := &d.H
	if h.Admissions[q.L] != 0 {
		prev := h.LastRoot[q.L] - 1
		reason := h.Certified[prev]
		last := w.Requests[prev]
		if reason == Negative && (!last.Answered || last.Answer == AOK) || reason == Original {
			h.Bad |= Twice
		}
		h.Twice[q.L] = reason
		h.Ledger[k] = reason
	}
	h.LastRoot[q.L] = k + 1
	h.Admissions[q.L]++
}

// T2 reads an unnamed start through the send/outstanding windows. Neither a
// lost request nor a send under an invalidated id can confirm an unnamed turn.
func (d *decision) turn(e Event) {
	r := d.M
	if !r.live() {
		return
	}
	switch e.K {
	case KTurnStart, KNamedStart:
		if r.compact() {
			r.Work = WkCompactTurn
		} else {
			r.Work = WkTurn
		}
		r.Phase = PhWork
		for k, a := range r.Att {
			if !a.open() || a.L != r.Task.L {
				continue
			}
			if e.K == KNamedStart {
				q := d.W.Requests[e.R]
				if a.Under != q.Under || a.L != q.L {
					continue
				}
			} else if e.R == Foreign || !(a.Stage == AtAcked || a.Stage == AtReg && d.W.Requests[k].Written && a.Under == r.Task.ID && d.Sending == uint8(k+1)) {
				continue
			}
			r.Task.Conf = 1
			r.News = a.News
			r.Att[k].TurnBy = 0
			refreshTurnBy(&r)
			d.H.Turned[r.Task.L] |= 1 << k // the oracle's own record of the credit
			break
		}
		if d.Restored && r.Task.L != 0 && r.RecoverBy != 0 {
			r.Task.Conf = 2
		}
		refreshTurnBy(&r)
	case KTurnEnd, KProbeIdle:
		if e.K == KProbeIdle && e.A == 1 {
			if r.RecoverBy != 0 {
				r.Task.Conf = 2
			}
			r.Phase, r.Work = PhWork, WkTurn
		} else if r.compact() {
			r.Work = WkCompact
		} else {
			r.Phase, r.Work = PhIdle, WkNone
		}
	}
	d.write(ClaimWrite, r, e)
}
func (d *decision) outcome(e Event) {
	k := e.R
	r := d.M
	if d.Sending != k+1 {
		return
	}
	d.Sending = 0
	if !r.Att[k].open() || r.Att[k].Under != r.Task.ID {
		return
	}
	switch e.A {
	case OcAck:
		r.Att[k].Stage = AtAcked
		d.W.Requests[k].Ack = true
		// Only this prompt's turn suppresses its timer. An earlier prompt's
		// confirmation can remain while a separately forwarded resend still
		// waits for its own turn (M3, D6).
		if !d.W.Requests[k].Started {
			r.Att[k].TurnBy = after(d.Now, 1)
			refreshTurnBy(&r)
		}
	case OcLost:
		r.Att[k].Stage = AtLost
		// D3: the loss takes the confirmation back only where nothing else
		// is outstanding. Another prompt still to answer keeps it (D5, A7).
		if r.Task.Conf != 0 && !r.othersOpen(int(k)) {
			r.Task.Conf = 0
		}
	case OcNoFrame:
		r.Att[k].Stage = AtClosed
	default:
		e.K = KRefusal
		if e.A == OcBusy {
			e.A = 1
		} else {
			e.A = 0
		}
		d.answer(e)
		return
	}
	d.write(DeliveryWrite, r, e)
}
func (d *decision) fail(r *Row) {
	take(r)
	invalidate(r)
	r.Phase, r.Work = PhFailed, WkNone
	r.Hold = Hold{}
	r.Intent = Intent{}
	r.Notify = true
}

// matches is whether a result is the exact answer of an attempt of the pending
// task that may settle it: the forwarded root the row records, under the id it
// was sent under (M2, M3, D9).
func (d *decision) matches(e Event) bool {
	r := d.M
	a := r.Att[e.R]
	q := d.W.Requests[e.R]
	return a.open() && a.L == r.Task.L && q.L == r.Task.L && q.Under == a.Under && q.Written && q.Root == e.R
}
func (d *decision) answer(e Event) {
	r := d.M
	k := e.R
	a := r.Att[k]
	q := d.W.Requests[k]
	if !r.live() || !a.open() || a.L != r.Task.L || !q.Written || q.L != a.L || q.Under != a.Under {
		return
	}
	waited := a.Under != r.Task.ID
	// No result is fanned out for a deduplicated request. Close only the
	// aliases of the forwarded prompt, cancelling any not-yet-written retry.
	if e.K == KResult {
		for j, b := range r.Att {
			if b.open() && b.L == a.L && b.Under == a.Under && (!d.W.Requests[j].Written || d.W.Requests[j].Root == k) {
				r.Att[j].Stage = AtClosed
				r.Att[j].TurnBy = 0
			}
		}
	} else {
		r.Att[k].Stage = AtClosed
		r.Att[k].TurnBy = 0
	}
	refreshTurnBy(&r)
	if !r.outstanding() {
		r.RecoverBy, r.TurnBy = 0, 0
	}
	w := EvidenceWrite
	switch {
	case e.K == KResult && e.A == AOK:
		r.News = a.News
		r.ErrRun = 0
		retire(&r, RServed, k)
		w = RetirementWrite
	case e.K == KResult && e.A == AFatal:
		d.fail(&r)
	default:
		busy := e.K == KResult && e.A == ABusyRes || e.K == KRefusal && e.B != 1 && (e.A == 1 || e.B == 2 || r.Phase == PhWork)
		if busy && (!waited || r.Task.Conf == 0) {
			r.Work, r.Phase = WkBusy, PhWork
		}
		if e.K == KResult && e.A == AProvErr {
			r.ErrRun++
		} else if !waited && !busy && !(e.K == KRefusal && e.B == 1) && r.Charges < ChargeLimit {
			r.Charges++
		}
		if !r.outstanding() {
			take(&r)
		} else if !waited {
			r.Task.ID++
		}
	}
	d.write(w, r, e)
}
func (d *decision) report(e Event) {
	r := d.M
	if !r.live() || e.B == 1 && r.ReportSeq != 0 {
		return
	}
	r.ReportSeq++
	if e.A == RpFatal {
		d.fail(&r)
	} else if e.A == RpClean {
		r.ErrRun = 0
	}
	d.write(EvidenceWrite, r, e)
}
