package steamworks

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ebitengine/purego"
)

// Flat-API symbols and their C signatures (steam_api.h / steam_api_flat.h).
// libsteam_api.so is C++ but these are extern "C", so the plain SysV calling
// convention applies. Type mapping used below:
//
//	PublishedFileId_t, SteamAPICall_t, uint64  -> uint64
//	uint32, AppId_t                             -> uint32
//	bool (C++, one byte, returned in al)        -> bool (purego reads the low byte)
//	ESteamAPIInitResult (enum, int-sized)       -> int32
//	interface pointers (ISteamUGC* ...)         -> uintptr
//	out-params (uint64*, char*, uint32*)        -> typed Go pointers, valid only during the call
const (
	// ESteamAPIInitResult SteamAPI_InitFlat( SteamErrMsg *pOutErrMsg );
	// SDK 1.58+. SteamErrMsg is char[1024]. 0 OK, 1 failed generic,
	// 2 no Steam client, 3 version mismatch.
	symInitFlat = "SteamAPI_InitFlat"
	// bool SteamAPI_Init(); exported by older SDKs (newer ones inline it).
	symInit = "SteamAPI_Init"
	// void SteamAPI_Shutdown();
	symShutdown = "SteamAPI_Shutdown"
	// void SteamAPI_RunCallbacks();
	symRunCallbacks = "SteamAPI_RunCallbacks"
	// ISteamUGC *SteamAPI_SteamUGC_vNNN(); loadAPI binds the newest one the
	// library exports, as found by checkSymbols.
	symUGCPrefix = "SteamAPI_SteamUGC_v"
	// SteamAPICall_t SteamAPI_ISteamUGC_SubscribeItem( ISteamUGC* self, PublishedFileId_t nPublishedFileID );
	symSubscribeItem = "SteamAPI_ISteamUGC_SubscribeItem"
	// bool SteamAPI_ISteamUGC_DownloadItem( ISteamUGC* self, PublishedFileId_t nPublishedFileID, bool bHighPriority );
	symDownloadItem = "SteamAPI_ISteamUGC_DownloadItem"
	// uint32 SteamAPI_ISteamUGC_GetItemState( ISteamUGC* self, PublishedFileId_t nPublishedFileID );
	symGetItemState = "SteamAPI_ISteamUGC_GetItemState"
	// bool SteamAPI_ISteamUGC_GetItemDownloadInfo( ISteamUGC* self, PublishedFileId_t nPublishedFileID,
	//     uint64 *punBytesDownloaded, uint64 *punBytesTotal );
	symGetItemDownloadInfo = "SteamAPI_ISteamUGC_GetItemDownloadInfo"
	// bool SteamAPI_ISteamUGC_GetItemInstallInfo( ISteamUGC* self, PublishedFileId_t nPublishedFileID,
	//     uint64 *punSizeOnDisk, char *pchFolder, uint32 cchFolderSize, uint32 *punTimeStamp );
	symGetItemInstallInfo = "SteamAPI_ISteamUGC_GetItemInstallInfo"

	// Optional, used only to explain failures.
	// ISteamUser *SteamAPI_SteamUser_vNNN();  bool SteamAPI_ISteamUser_BLoggedOn( ISteamUser* self );
	symUserPrefix = "SteamAPI_SteamUser_v"
	symBLoggedOn  = "SteamAPI_ISteamUser_BLoggedOn"
	// ISteamApps *SteamAPI_SteamApps_vNNN();  bool SteamAPI_ISteamApps_BIsSubscribed( ISteamApps* self );
	symAppsPrefix   = "SteamAPI_SteamApps_v"
	symBIsSubscribe = "SteamAPI_ISteamApps_BIsSubscribed"
)

// requiredSymbols must all be exported (besides one init and one UGC accessor).
var requiredSymbols = []string{
	symShutdown, symRunCallbacks, symSubscribeItem, symDownloadItem,
	symGetItemState, symGetItemDownloadInfo, symGetItemInstallInfo,
}

// Version ranges probed, newest first, for the optional diagnostic
// accessors. ISteamUGC is not probed: checkSymbols finds its accessor.
var (
	userVersions = versionRange(40, 19)
	appsVersions = versionRange(20, 8)
)

func versionRange(hi, lo int) []int {
	var v []int
	for i := hi; i >= lo; i-- {
		v = append(v, i)
	}
	return v
}

// EItemState flags from GetItemState.
const (
	ItemSubscribed      uint32 = 1
	ItemLegacy          uint32 = 2
	ItemInstalled       uint32 = 4
	ItemNeedsUpdate     uint32 = 8
	ItemDownloading     uint32 = 16
	ItemDownloadPending uint32 = 32
)

// itemBusy are the flags that mean the client still has work to do.
const itemBusy = ItemNeedsUpdate | ItemDownloading | ItemDownloadPending

// Init failure kinds.
var (
	ErrNoSteamClient   = errors.New("the Steam client is not running")
	ErrVersionMismatch = errors.New("libsteam_api.so is too old for the running Steam client")
	ErrNotLoggedIn     = errors.New("the Steam client is not logged in (or is in offline mode)")
	ErrNotOwned        = errors.New("the logged-in Steam account does not own DayZ")
)

// steamAPI is a loaded libsteam_api.so.
type steamAPI struct {
	ugc           uintptr // ISteamUGC*, valid after init
	ugcSymbol     string  // the SteamAPI_SteamUGC_vNNN in use
	ugcAccessorFn func() uintptr

	initFlat     func(errMsg *byte) int32
	initOld      func() bool
	shutdown     func()
	runCallbacks func()

	subscribeItem       func(self uintptr, id uint64) uint64
	downloadItem        func(self uintptr, id uint64, highPriority bool) bool
	getItemState        func(self uintptr, id uint64) uint32
	getItemDownloadInfo func(self uintptr, id uint64, downloaded, total *uint64) bool
	getItemInstallInfo  func(self uintptr, id uint64, sizeOnDisk *uint64, folder *byte, folderSize uint32, timestamp *uint32) bool

	// Optional diagnostics; nil when not exported.
	userAccessor func() uintptr
	bLoggedOn    func(self uintptr) bool
	appsAccessor func() uintptr
	bIsSubscribe func(self uintptr) bool
}

// loadAPI checks path like FindLibrary does, dlopens it and binds every
// symbol. It does not initialise Steam.
func loadAPI(path string) (*steamAPI, error) {
	info, err := inspectLibrary(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return bindAPI(path, info, func(name string) uintptr {
		sym, err := purego.Dlsym(h, name)
		if err != nil {
			return 0
		}
		return sym
	})
}

// bindAPI binds the symbols that lookup resolves (0 means missing). The
// ISteamUGC accessor is exactly info.ugcSymbol, the one checkSymbols picked.
func bindAPI(path string, info libInfo, lookup func(name string) uintptr) (*steamAPI, error) {
	a := &steamAPI{ugcSymbol: info.ugcSymbol}
	bind := func(fptr any, name string, required bool) error {
		sym := lookup(name)
		if sym == 0 {
			if required {
				return fmt.Errorf("%s: missing %s", path, name)
			}
			return nil
		}
		purego.RegisterFunc(fptr, sym)
		return nil
	}
	_ = bind(&a.initFlat, symInitFlat, false)
	_ = bind(&a.initOld, symInit, false)
	if a.initFlat == nil && a.initOld == nil {
		return nil, fmt.Errorf("%s: exports neither %s nor %s", path, symInitFlat, symInit)
	}
	for _, b := range []struct {
		fptr any
		name string
	}{
		{&a.shutdown, symShutdown},
		{&a.runCallbacks, symRunCallbacks},
		{&a.subscribeItem, symSubscribeItem},
		{&a.downloadItem, symDownloadItem},
		{&a.getItemState, symGetItemState},
		{&a.getItemDownloadInfo, symGetItemDownloadInfo},
		{&a.getItemInstallInfo, symGetItemInstallInfo},
		{&a.ugcAccessorFn, info.ugcSymbol},
	} {
		if err := bind(b.fptr, b.name, true); err != nil {
			return nil, err
		}
	}
	a.userAccessor = probeAccessor(lookup, symUserPrefix, userVersions)
	a.appsAccessor = probeAccessor(lookup, symAppsPrefix, appsVersions)
	_ = bind(&a.bLoggedOn, symBLoggedOn, false)
	_ = bind(&a.bIsSubscribe, symBIsSubscribe, false)
	return a, nil
}

func probeAccessor(lookup func(string) uintptr, prefix string, versions []int) func() uintptr {
	for _, v := range versions {
		if sym := lookup(fmt.Sprintf("%s%03d", prefix, v)); sym != 0 {
			var fn func() uintptr
			purego.RegisterFunc(&fn, sym)
			return fn
		}
	}
	return nil
}

// init starts the Steam API (SteamAppId must already be in the environment)
// and fetches the ISteamUGC interface. Failures are mapped to the Err* kinds.
func (a *steamAPI) init() error {
	if a.initFlat != nil {
		var msg [1024]byte
		res := a.initFlat(&msg[0])
		text := cString(msg[:])
		if res != 0 {
			return initError(res, text)
		}
	} else if !a.initOld() {
		return fmt.Errorf("SteamAPI_Init failed: Steam not running, not logged in, or the account does not own DayZ")
	}
	if a.userAccessor != nil && a.bLoggedOn != nil {
		if u := a.userAccessor(); u != 0 && !a.bLoggedOn(u) {
			a.shutdown()
			return ErrNotLoggedIn
		}
	}
	if a.appsAccessor != nil && a.bIsSubscribe != nil {
		if apps := a.appsAccessor(); apps != 0 && !a.bIsSubscribe(apps) {
			a.shutdown()
			return ErrNotOwned
		}
	}
	a.ugc = a.ugcAccessorFn()
	if a.ugc == 0 {
		a.shutdown()
		return fmt.Errorf("%s returned NULL (interface not supported by this Steam client)", a.ugcSymbol)
	}
	return nil
}

// initError maps an ESteamAPIInitResult and its message to an error.
func initError(res int32, msg string) error {
	var kind error
	switch res {
	case 2:
		kind = ErrNoSteamClient
	case 3:
		kind = ErrVersionMismatch
	default:
		low := bytes.ToLower([]byte(msg))
		switch {
		case bytes.Contains(low, []byte("logged")) || bytes.Contains(low, []byte("logon")):
			kind = ErrNotLoggedIn
		case bytes.Contains(low, []byte("own")) || bytes.Contains(low, []byte("license")) ||
			bytes.Contains(low, []byte("subscri")):
			kind = ErrNotOwned
		default:
			if msg == "" {
				return fmt.Errorf("SteamAPI_InitFlat failed (result %d)", res)
			}
			return fmt.Errorf("SteamAPI_InitFlat failed: %s", msg)
		}
	}
	if msg != "" {
		return fmt.Errorf("%w (%s)", kind, msg)
	}
	return kind
}

// cString returns the NUL-terminated prefix of b.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func (a *steamAPI) state(id uint64) uint32 { return a.getItemState(a.ugc, id) }

func (a *steamAPI) progress(id uint64) (downloaded, total uint64) {
	if !a.getItemDownloadInfo(a.ugc, id, &downloaded, &total) {
		return 0, 0
	}
	return downloaded, total
}

func (a *steamAPI) installFolder(id uint64) string {
	var size uint64
	var ts uint32
	var buf [4096]byte
	if !a.getItemInstallInfo(a.ugc, id, &size, &buf[0], uint32(len(buf)), &ts) {
		return ""
	}
	return cString(buf[:])
}
