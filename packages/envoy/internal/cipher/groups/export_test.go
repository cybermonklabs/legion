package groups

import "time"

// SetEvery makes Run read at every tick of every, so a test sees ticks without waiting an hour.
func SetEvery(r *Reader, every time.Duration) { r.every = every }

// SetReadTimeout makes one group's read give up after timeout, so a test sees a hung read end.
func SetReadTimeout(r *Reader, timeout time.Duration) { r.readTimeout = timeout }

// HasMember answers whether login is a member of group by its last good read, when that read is
// fresh: Membership's two answers, both true.
func (r *Reader) HasMember(group, login string) bool {
	listed, fresh := r.Membership(group, login)
	return listed && fresh
}

// Fresh answers whether group's last good read is fresh: Membership's second answer.
func (r *Reader) Fresh(group string) bool {
	_, fresh := r.Membership(group, "")
	return fresh
}
