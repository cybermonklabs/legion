package leakguard

// fionread is the ioctl that answers how many bytes a pipe's buffer holds (pipeReader.drain):
// FIONREAD, _IOR('f', 127, int) in darwin's <sys/filio.h>, which golang.org/x/sys/unix does not
// name for darwin.
const fionread = 0x4004667f
