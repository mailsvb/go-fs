//go:build !light && !no_nbd

package nbd

// The numbers of the NBD protocol, as doc/proto.md of the NBD project names
// them. Only fixed newstyle negotiation is spoken: oldstyle has no options and
// so no TLS, and plain newstyle predates the error replies this server needs.

// handshake
const (
	nbdMagic   uint64 = 0x4e42444d41474943 // "NBDMAGIC"
	optMagic   uint64 = 0x49484156454f5054 // "IHAVEOPT"
	replyMagic uint64 = 0x0003e889045565a9

	flagFixedNewstyle uint16 = 1 << 0
	flagNoZeroes      uint16 = 1 << 1

	clientFlagFixedNewstyle uint32 = 1 << 0
	clientFlagNoZeroes      uint32 = 1 << 1
)

// options
const (
	optExportName      uint32 = 1
	optAbort           uint32 = 2
	optList            uint32 = 3
	optStartTLS        uint32 = 5
	optInfo            uint32 = 6
	optGo              uint32 = 7
	optStructuredReply uint32 = 8
	optListMetaContext uint32 = 9
	optSetMetaContext  uint32 = 10
	optExtendedHeaders uint32 = 11
)

// option replies
const (
	repAck         uint32 = 1
	repServer      uint32 = 2
	repInfo        uint32 = 3
	repMetaContext uint32 = 4

	repFlagError   uint32 = 1 << 31
	repErrUnsup           = repFlagError | 1
	repErrPolicy          = repFlagError | 2
	repErrInvalid         = repFlagError | 3
	repErrPlatform        = repFlagError | 4
	repErrTLSReqd         = repFlagError | 5
	repErrUnknown         = repFlagError | 6
	repErrShutdown        = repFlagError | 7
	repErrTooBig          = repFlagError | 9
)

// what NBD_OPT_INFO and NBD_OPT_GO report
const (
	infoExport    uint16 = 0
	infoName      uint16 = 1
	infoBlockSize uint16 = 3
)

// transmission flags
const (
	tflagHasFlags     uint16 = 1 << 0
	tflagReadOnly     uint16 = 1 << 1
	tflagSendFlush    uint16 = 1 << 2
	tflagSendFUA      uint16 = 1 << 3
	tflagSendTrim     uint16 = 1 << 5
	tflagWriteZeroes  uint16 = 1 << 6
	tflagSendDF       uint16 = 1 << 7
	tflagMultiConn    uint16 = 1 << 8
	tflagSendCache    uint16 = 1 << 10
	tflagSendFastZero uint16 = 1 << 11
)

// transmission
const (
	requestMagic    uint32 = 0x25609513
	simpleMagic     uint32 = 0x67446698
	structuredMagic uint32 = 0x668e33ef

	cmdRead        uint16 = 0
	cmdWrite       uint16 = 1
	cmdDisc        uint16 = 2
	cmdFlush       uint16 = 3
	cmdTrim        uint16 = 4
	cmdCache       uint16 = 5
	cmdWriteZeroes uint16 = 6
	cmdBlockStatus uint16 = 7

	cmdFlagFUA      uint16 = 1 << 0
	cmdFlagNoHole   uint16 = 1 << 1
	cmdFlagDF       uint16 = 1 << 2
	cmdFlagReqOne   uint16 = 1 << 3
	cmdFlagFastZero uint16 = 1 << 4
	cmdFlagsKnown          = cmdFlagFUA | cmdFlagNoHole | cmdFlagDF | cmdFlagReqOne | cmdFlagFastZero

	replyFlagDone uint16 = 1 << 0

	replyTypeNone        uint16 = 0
	replyTypeOffsetData  uint16 = 1
	replyTypeBlockStatus uint16 = 5
	replyTypeError       uint16 = 1<<15 + 1
	replyTypeErrorOffset uint16 = 1<<15 + 2
)

// the errors a request fails with, which are the Linux errno values
const (
	errPerm     uint32 = 1
	errIO       uint32 = 5
	errNoMem    uint32 = 12
	errInvalid  uint32 = 22
	errNoSpace  uint32 = 28
	errOverflow uint32 = 75
	errNotSup   uint32 = 95
	errShutdown uint32 = 108
)

// base:allocation, the one metadata context: which blocks are holes and
// which read as zeros
const (
	contextBaseAllocation        = "base:allocation"
	contextBaseID         uint32 = 1

	stateHole uint32 = 1 << 0
	stateZero uint32 = 1 << 1
)

// limits
const (
	// minBlock, preferredBlock and maxBlock are what NBD_INFO_BLOCK_SIZE
	// says: any request is fine, a whole page is best, and nothing above
	// 32 MiB is taken.
	minBlock       = 1
	preferredBlock = 4096
	maxBlock       = 32 << 20
	// maxOption is the largest option the server reads; an export name is
	// at most 4096 bytes, and nothing it understands needs more.
	maxOption = 64 << 10
	// maxNameLength is the longest export name the protocol allows.
	maxNameLength = 4096
	// maxInFlight and inFlightBytes bound what one connection may have
	// pending: QEMU keeps up to 16 requests in flight.
	maxInFlight   = 16
	inFlightBytes = 64 << 20
	// maxExtents bounds one block status reply; a client asks again for
	// what it left out.
	maxExtents = 1 << 16
)
