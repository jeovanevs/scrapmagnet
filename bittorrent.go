package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path"
	"strings"
	"time"

	"github.com/sharkone/libtorrent-go"
)

type TorrentFileInfo struct {
	Path           string   `json:"path"`
	Size           int64    `json:"size"`
	CompletePieces int      `json:"complete_pieces"`
	TotalPieces    int      `json:"total_pieces"`
	PieceMap       []string `json:"piece_map"`

	handle      libtorrent.Torrent_handle
	offset      int64
	pieceLength int
	startPiece  int
	endPiece    int

	file      *os.File
	bytesRead int
}

func NewTorrentFileInfo(path string, size int64, offset int64, pieceLength int, handle libtorrent.Torrent_handle) *TorrentFileInfo {
	result := &TorrentFileInfo{}
	result.Path = path
	result.Size = size
	result.offset = offset
	result.pieceLength = pieceLength
	result.handle = handle
	result.startPiece = result.GetPieceIndexFromOffset(0)
	result.endPiece = result.GetPieceIndexFromOffset(size)
	result.CompletePieces = result.GetCompletePieces()
	result.TotalPieces = 1 + result.endPiece - result.startPiece
	result.PieceMap = result.GetPieceMap()
	return result
}

func (tfi *TorrentFileInfo) GetInfoHashStr() string {
	return bitTorrent.getTorrentInfoHash(tfi.handle)
}

func (tfi *TorrentFileInfo) GetPieceIndexFromOffset(offset int64) int {
	pieceIndex := int((tfi.offset + offset) / int64(tfi.pieceLength))
	return pieceIndex
}

func (tfi *TorrentFileInfo) GetCompletePieces() int {
	completePieces := 0
	for i := tfi.startPiece; i <= tfi.endPiece; i++ {
		if tfi.handle.Have_piece(i) {
			completePieces += 1
		}
	}
	return completePieces
}

func (tfi *TorrentFileInfo) GetPieceMap() []string {
	totalRows := tfi.TotalPieces / 100
	if (tfi.TotalPieces % 100) != 0 {
		totalRows++
	}

	result := make([]string, totalRows)
	for i := tfi.startPiece; i <= tfi.endPiece; i++ {
		if tfi.handle.Have_piece(i) {
			result[(i-tfi.startPiece)/100] += "*"
		} else {
			result[(i-tfi.startPiece)/100] += fmt.Sprintf("%v", tfi.handle.Piece_priority(i))
		}
	}
	return result
}

func (tfi *TorrentFileInfo) SetInitialPriority() {
	start := tfi.startPiece
	end := int(math.Min(float64(start+tfi.getLookAhead(true)), float64(tfi.endPiece)))
	for i := start; i <= end; i++ {
		tfi.handle.Set_piece_deadline(i, 10000, 0)
	}

	tfi.handle.Set_piece_deadline(tfi.endPiece, 10000, 0)
}

func (tfi *TorrentFileInfo) IsVideoReady() bool {
	start := tfi.startPiece
	end := int(math.Min(float64(start+tfi.getLookAhead(true)), float64(tfi.endPiece)))
	for i := start; i <= end; i++ {
		if !tfi.handle.Have_piece(i) {
			return false
		}
	}

	if !tfi.handle.Have_piece(tfi.endPiece) {
		return false
	}

	return true
}

func (tfi *TorrentFileInfo) Open(downloadDir string) bool {
	if tfi.file == nil {
		fullpath := path.Join(downloadDir, tfi.Path)

		for {
			if _, err := os.Stat(fullpath); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		tfi.file, _ = os.Open(fullpath)
		tfi.bytesRead = 0
	}

	return tfi.file != nil
}

func (tfi *TorrentFileInfo) Close() {
	if tfi.file != nil {
		tfi.file.Close()
	}
}

func (tfi *TorrentFileInfo) Read(data []byte) (int, error) {
	totalRead := 0
	size := len(data)

	for size > 0 {
		readSize := int64(math.Min(float64(size), float64(tfi.pieceLength)))

		currentPosition, _ := tfi.file.Seek(0, os.SEEK_CUR)
		pieceIndex := tfi.GetPieceIndexFromOffset(currentPosition + readSize)
		tfi.waitForPiece(pieceIndex, false)

		tmpData := make([]byte, readSize)
		read, err := tfi.file.Read(tmpData)
		if err != nil {
			totalRead += read
			log.Println("[scrapmagnet] Read failed", read, readSize, currentPosition, err)
			return totalRead, err
		}

		copy(data[totalRead:], tmpData[:read])
		totalRead += read
		size -= read
	}

	tfi.bytesRead += totalRead

	if tfi.bytesRead > (10*1024*1024) && !bitTorrent.connectionInfos[tfi.GetInfoHashStr()].Served {
		log.Printf("[scrapmagnet] Serving %v", tfi.handle.Status().GetName())
		trackingEvent("Serving", map[string]interface{}{"Magnet InfoHash": tfi.GetInfoHashStr(), "Magnet Name": tfi.handle.Status().GetName()}, bitTorrent.mixpanelData[tfi.GetInfoHashStr()])
		bitTorrent.connectionInfos[tfi.GetInfoHashStr()].Served = true
	}

	return totalRead, nil
}

func (tfi *TorrentFileInfo) Seek(offset int64, whence int) (int64, error) {
	newPosition := int64(0)

	switch whence {
	case os.SEEK_SET:
		newPosition = offset
	case os.SEEK_CUR:
		currentPosition, _ := tfi.file.Seek(0, os.SEEK_CUR)
		newPosition = currentPosition + offset
	case os.SEEK_END:
		newPosition = tfi.Size + offset
	}

	pieceIndex := tfi.GetPieceIndexFromOffset(newPosition)
	tfi.waitForPiece(pieceIndex, true)

	ret, err := tfi.file.Seek(offset, whence)
	if err != nil || ret != newPosition {
		log.Print("[scrapmagnet] Seek failed", ret, newPosition, err)
	}

	return ret, err
}

func (tfi *TorrentFileInfo) waitForPiece(pieceIndex int, timeCritical bool) bool {
	if !tfi.handle.Have_piece(pieceIndex) {
		if timeCritical {
			tfi.handle.Clear_piece_deadlines()
			for i := 0; i <= tfi.getLookAhead(false) && (i+pieceIndex) <= tfi.endPiece; i++ {
				tfi.handle.Set_piece_deadline(pieceIndex+i, 3000+i*1000, 0)
			}
		} else {
			for i := tfi.startPiece; i < tfi.endPiece; i++ {
				tfi.handle.Piece_priority(i, 1)
			}
			for i := 0; i <= tfi.getLookAhead(false)*4 && (i+pieceIndex) <= tfi.endPiece; i++ {
				tfi.handle.Piece_priority(pieceIndex+i, 7)
			}
		}

		tfi.SetInitialPriority()

		for {
			time.Sleep(100 * time.Millisecond)
			if tfi.handle.Have_piece(pieceIndex) {
				break
			}
		}
	}

	return false
}

func (tfi *TorrentFileInfo) getLookAhead(initial bool) int {
	if initial {
		return int(float32(tfi.TotalPieces) * bitTorrent.lookAhead[tfi.GetInfoHashStr()])
	}
	return int(float32(tfi.TotalPieces) * 0.005)
}

type TorrentInfo struct {
	Name         string             `json:"name"`
	InfoHash     string             `json:"info_hash"`
	DownloadDir  string             `json:"download_dir"`
	State        int                `json:"state"`
	StateStr     string             `json:"state_str"`
	Paused       bool               `json:"paused"`
	Size         int64              `json:"size"`
	Pieces       int                `json:"pieces"`
	Progress     float32            `json:"progress"`
	DownloadRate int                `json:"download_rate"`
	UploadRate   int                `json:"upload_rate"`
	Seeds        int                `json:"seeds"`
	TotalSeeds   int                `json:"total_seeds"`
	Peers        int                `json:"peers"`
	TotalPeers   int                `json:"total_peers"`
	Files        []*TorrentFileInfo `json:"files"`

	ConnectionInfo *TorrentConnectionInfo `json:"connection_info"`
}

func NewTorrentInfo(handle libtorrent.Torrent_handle) (result *TorrentInfo) {
	result = &TorrentInfo{}

	torrentStatus := handle.Status()

	result.InfoHash = bitTorrent.getTorrentInfoHash(handle)
	result.Name = torrentStatus.GetName()
	result.DownloadDir = torrentStatus.GetSave_path()
	result.State = int(torrentStatus.GetState())
	result.StateStr = func(state libtorrent.LibtorrentTorrent_statusState_t) string {
		switch state {
		case libtorrent.Torrent_statusQueued_for_checking:
			return "Queued for checking"
		case libtorrent.Torrent_statusChecking_files:
			return "Checking files"
		case libtorrent.Torrent_statusDownloading_metadata:
			return "Downloading metadata"
		case libtorrent.Torrent_statusDownloading:
			return "Downloading"
		case libtorrent.Torrent_statusFinished:
			return "Finished"
		case libtorrent.Torrent_statusSeeding:
			return "Seeding"
		case libtorrent.Torrent_statusAllocating:
			return "Allocating"
		case libtorrent.Torrent_statusChecking_resume_data:
			return "Checking resume data"
		default:
			return "Unknown"
		}
	}(torrentStatus.GetState())
	result.Paused = torrentStatus.GetPaused()
	result.Progress = torrentStatus.GetProgress()
	result.DownloadRate = torrentStatus.GetDownload_rate() / 1024
	result.UploadRate = torrentStatus.GetUpload_rate() / 1024
	result.Seeds = torrentStatus.GetNum_seeds()
	result.TotalSeeds = torrentStatus.GetNum_complete()
	result.Peers = torrentStatus.GetNum_peers()
	result.TotalPeers = torrentStatus.GetNum_incomplete()

	torrentInfo := handle.Torrent_file()
	if torrentInfo.Swigcptr() != 0 {
		result.Files = func(torrentInfo libtorrent.Torrent_info) (result []*TorrentFileInfo) {
			for i := 0; i < torrentInfo.Files().Num_files(); i++ {
				result = append(result, NewTorrentFileInfo(torrentInfo.Files().File_path(i), torrentInfo.Files().File_size(i), torrentInfo.Files().File_offset(i), torrentInfo.Piece_length(), handle))
			}
			return result
		}(torrentInfo)
		result.Size = torrentInfo.Files().Total_size()
		result.Pieces = torrentInfo.Num_pieces()
	}

	result.ConnectionInfo = bitTorrent.connectionInfos[result.InfoHash]
	return result
}

func (ti *TorrentInfo) GetTorrentFileInfo(filePath string) *TorrentFileInfo {
	for _, torrentFileInfo := range ti.Files {
		if torrentFileInfo.Path == filePath {
			return torrentFileInfo
		}
	}
	return nil
}

func (ti *TorrentInfo) GetBiggestTorrentFileInfo() (result *TorrentFileInfo) {
	for _, torrentFileInfo := range ti.Files {
		if result == nil || torrentFileInfo.Size > result.Size {
			result = torrentFileInfo
		}
	}
	return result
}

type TorrentConnectionInfo struct {
	connectionChan  chan int
	ConnectionCount int  `json:"connection_count"`
	Served          bool `json:"served"`
	paused          bool
}

func NewTorrentConnectionInfo() *TorrentConnectionInfo {
	return &TorrentConnectionInfo{
		connectionChan:  make(chan int),
		ConnectionCount: 0,
		Served:          false,
		paused:          false,
	}
}

type BitTorrent struct {
	session         libtorrent.Session
	lookAhead       map[string]float32
	mixpanelData    map[string]string
	connectionInfos map[string]*TorrentConnectionInfo
	removeChan      chan bool
	deleteChan      chan bool
}

func NewBitTorrent() *BitTorrent {
	return &BitTorrent{
		lookAhead:       make(map[string]float32),
		mixpanelData:    make(map[string]string),
		connectionInfos: make(map[string]*TorrentConnectionInfo),
		removeChan:      make(chan bool),
		deleteChan:      make(chan bool),
	}
}

func (b *BitTorrent) Start() {
	peopleSet()

	fingerprint := libtorrent.NewFingerprint("LT", libtorrent.LIBTORRENT_VERSION_MAJOR, libtorrent.LIBTORRENT_VERSION_MINOR, 0, 0)
	sessionFlags := int(libtorrent.SessionAdd_default_plugins)
	alertMask := uint(libtorrent.AlertError_notification | libtorrent.AlertStorage_notification | libtorrent.AlertStatus_notification)

	b.session = libtorrent.NewSession(fingerprint, sessionFlags)
	b.session.Set_alert_mask(alertMask)
	go b.alertPump()

	sessionSettings := b.session.Settings()
	sessionSettings.SetAnnounce_to_all_tiers(true)
	sessionSettings.SetAnnounce_to_all_trackers(true)
	sessionSettings.SetConnection_speed(100)
	sessionSettings.SetPeer_connect_timeout(2)
	sessionSettings.SetRate_limit_ip_overhead(true)
	sessionSettings.SetRequest_timeout(5)
	sessionSettings.SetTorrent_connect_boost(100)
	if settings.maxDownloadRate > 0 {
		sessionSettings.SetDownload_rate_limit(settings.maxDownloadRate * 1024)
	}
	if settings.maxUploadRate > 0 {
		sessionSettings.SetUpload_rate_limit(settings.maxUploadRate * 1024)
	}
	b.session.Set_settings(sessionSettings)

	proxySettings := libtorrent.NewProxy_settings()
	if settings.proxyType == "SOCKS5" {
		proxySettings.SetHostname(settings.proxyHost)
		proxySettings.SetPort(uint16(settings.proxyPort))
		if settings.proxyUser != "" {
			proxySettings.SetXtype(byte(libtorrent.Proxy_settingsSocks5_pw))
			proxySettings.SetUsername(settings.proxyUser)
			proxySettings.SetPassword(settings.proxyPassword)
		} else {
			proxySettings.SetXtype(byte(libtorrent.Proxy_settingsSocks5))
		}
	}
	b.session.Set_proxy(proxySettings)

	encryptionSettings := libtorrent.NewPe_settings()
	encryptionSettings.SetOut_enc_policy(byte(libtorrent.Pe_settingsForced))
	encryptionSettings.SetIn_enc_policy(byte(libtorrent.Pe_settingsForced))
	encryptionSettings.SetAllowed_enc_level(byte(libtorrent.Pe_settingsBoth))
	encryptionSettings.SetPrefer_rc4(true)
	b.session.Set_pe_settings(encryptionSettings)

	ec := libtorrent.NewError_code()
	b.session.Listen_on(libtorrent.NewStd_pair_int_int(settings.bitTorrentPort, settings.bitTorrentPort), ec)

	b.session.Start_dht()
	b.session.Start_lsd()

	if settings.uPNPNatPMPEnabled {
		b.session.Start_upnp()
		b.session.Start_natpmp()
	}
}

func (b *BitTorrent) Stop() {
	for i := 0; i < int(b.session.Get_torrents().Size()); i++ {
		b.removeTorrent(b.session.Get_torrents().Get(i))
	}

	if settings.uPNPNatPMPEnabled {
		b.session.Stop_natpmp()
		b.session.Stop_upnp()
	}

	b.session.Stop_lsd()
	b.session.Stop_dht()
}

func (b *BitTorrent) AddTorrent(magnetLink string, downloadDir string, infoHash string, lookAhead float32, mixpanelData string) {
	addTorrentParams := libtorrent.NewAdd_torrent_params()
	addTorrentParams.SetUrl(magnetLink)
	addTorrentParams.SetSave_path(downloadDir)
	addTorrentParams.SetStorage_mode(libtorrent.Storage_mode_sparse)
	addTorrentParams.SetFlags(0)

	if _, ok := b.lookAhead[infoHash]; !ok {
		b.lookAhead[infoHash] = lookAhead
	}

	if _, ok := b.mixpanelData[infoHash]; !ok {
		b.mixpanelData[infoHash] = mixpanelData
	}

	b.session.Async_add_torrent(addTorrentParams)
}

func (b *BitTorrent) GetTorrentInfos() (result []*TorrentInfo) {
	result = make([]*TorrentInfo, 0, 0)
	handles := b.session.Get_torrents()
	for i := 0; i < int(handles.Size()); i++ {
		if _, ok := b.connectionInfos[b.getTorrentInfoHash(handles.Get(i))]; ok {
			result = append(result, NewTorrentInfo(handles.Get(i)))
		}
	}
	return result
}

func (b *BitTorrent) GetTorrentInfo(infoHash string) *TorrentInfo {
	handles := b.session.Get_torrents()
	for i := 0; i < int(handles.Size()); i++ {
		if infoHash == b.getTorrentInfoHash(handles.Get(i)) {
			if _, ok := b.connectionInfos[infoHash]; ok {
				return NewTorrentInfo(handles.Get(i))
			}
		}
	}
	return nil
}

func (b *BitTorrent) AddConnection(infoHash string) {
	b.connectionInfos[infoHash].connectionChan <- 1
}

func (b *BitTorrent) RemoveConnection(infoHash string) {
	b.connectionInfos[infoHash].connectionChan <- -1
}

func (b *BitTorrent) getTorrentInfoHash(handle libtorrent.Torrent_handle) string {
	return fmt.Sprintf("%X", handle.Info_hash().To_string())
}

func (b *BitTorrent) pauseTorrent(handle libtorrent.Torrent_handle) {
	handle.Pause()
}

func (b *BitTorrent) resumeTorrent(handle libtorrent.Torrent_handle) {
	handle.Resume()
}

func (b *BitTorrent) removeTorrent(handle libtorrent.Torrent_handle) {
	removeFlags := 0
	if !settings.keepFiles {
		removeFlags |= int(libtorrent.SessionDelete_files)
	}

	b.session.Remove_torrent(handle, removeFlags)
	<-b.removeChan

	if (removeFlags & int(libtorrent.SessionDelete_files)) != 0 {
		<-b.deleteChan
	}
}

func (b *BitTorrent) alertPump() {
	for {
		if b.session.Wait_for_alert(libtorrent.Seconds(1)).Swigcptr() != 0 {
			alert := b.session.Pop_alert()
			switch alert.Xtype() {
			case libtorrent.Torrent_added_alertAlert_type:
				torrentAddedAlert := libtorrent.SwigcptrTorrent_added_alert(alert.Swigcptr())
				b.onTorrentAdded(torrentAddedAlert.GetHandle())
			case libtorrent.Metadata_received_alertAlert_type:
				metadataReceivedAlert := libtorrent.SwigcptrMetadata_received_alert(alert.Swigcptr())
				b.onMetadataReceived(metadataReceivedAlert.GetHandle())
			case libtorrent.Torrent_paused_alertAlert_type:
				torrentPausedAlert := libtorrent.SwigcptrTorrent_paused_alert(alert.Swigcptr())
				b.onTorrentPaused(torrentPausedAlert.GetHandle())
			case libtorrent.Torrent_resumed_alertAlert_type:
				torrentResumedAlert := libtorrent.SwigcptrTorrent_resumed_alert(alert.Swigcptr())
				b.onTorrentResumed(torrentResumedAlert.GetHandle())
			case libtorrent.Torrent_finished_alertAlert_type:
				torrentFinishedAlert := libtorrent.SwigcptrTorrent_finished_alert(alert.Swigcptr())
				b.onTorrentFinished(torrentFinishedAlert.GetHandle())
			case libtorrent.Torrent_removed_alertAlert_type:
				torrentRemovedAlert := libtorrent.SwigcptrTorrent_removed_alert(alert.Swigcptr())
				b.onTorrentRemoved(torrentRemovedAlert.GetHandle())
			case libtorrent.Torrent_deleted_alertAlert_type:
				torrentDeletedAlert := libtorrent.SwigcptrTorrent_deleted_alert(alert.Swigcptr())
				b.onTorrentDeleted(torrentDeletedAlert.GetInfo_hash().To_string(), true)
			case libtorrent.Torrent_delete_failed_alertAlert_type:
				torrentDeletedAlert := libtorrent.SwigcptrTorrent_deleted_alert(alert.Swigcptr())
				b.onTorrentDeleted(torrentDeletedAlert.GetInfo_hash().To_string(), false)
			case libtorrent.Listen_succeeded_alertAlert_type:
				listenSucceedAlert := libtorrent.SwigcptrListen_succeeded_alert(alert.Swigcptr())
				if listenSucceedAlert.GetSock_type() != libtorrent.Listen_succeeded_alertTcp_ssl && !strings.Contains(listenSucceedAlert.Message(), "[::]") {
					log.Printf("[scrapmagnet] %s", listenSucceedAlert.Message())
				}
			case libtorrent.Add_torrent_alertAlert_type:
				// Ignore
			case libtorrent.Torrent_checked_alertAlert_type:
				// Ignore
			case libtorrent.State_changed_alertAlert_type:
				// Ignore
			case libtorrent.Hash_failed_alertAlert_type:
				// Ignore
			case libtorrent.Cache_flushed_alertAlert_type:
				// Ignore
			case libtorrent.External_ip_alertAlert_type:
				// Ignore
			case libtorrent.Portmap_error_alertAlert_type:
				// Ignore
			case libtorrent.Tracker_error_alertAlert_type:
				// Ignore
			case libtorrent.Udp_error_alertAlert_type:
				// Ignore
			default:
				log.Printf("[scrapmagnet] %s: %s", alert.What(), alert.Message())
			}
		}
	}
}

func (b *BitTorrent) onTorrentAdded(handle libtorrent.Torrent_handle) {
	infoHash := b.getTorrentInfoHash(handle)

	b.connectionInfos[infoHash] = NewTorrentConnectionInfo()

	go func() {
		watcherRunning := false
		resumeChan := make(chan bool)

		// Auto pause/remove
		for {
			b.connectionInfos[infoHash].ConnectionCount += <-b.connectionInfos[infoHash].connectionChan
			if b.connectionInfos[infoHash].ConnectionCount > 0 {
				if watcherRunning {
					resumeChan <- true
				}
			} else {
				go func() {
					watcherRunning = true
					paused := false
				Watcher:
					for {
						if !paused {
							select {
							case <-resumeChan:
								break Watcher
							case <-time.After(time.Duration(settings.inactivityPauseTimeout) * time.Second):
								b.pauseTorrent(handle)
								paused = true
							}
						} else {
							select {
							case <-resumeChan:
								b.resumeTorrent(handle)
								break Watcher
							case <-time.After(time.Duration(settings.inactivityRemoveTimeout) * time.Second):
								b.removeTorrent(handle)
								break Watcher
							}
						}
					}
					watcherRunning = false
				}()
			}
		}
	}()

	log.Printf("[scrapmagnet] Added %v", handle.Status().GetName())
	trackingEvent("Added", map[string]interface{}{"Magnet InfoHash": b.getTorrentInfoHash(handle), "Magnet Name": handle.Status().GetName()}, b.mixpanelData[infoHash])
}

func (b *BitTorrent) onMetadataReceived(handle libtorrent.Torrent_handle) {
	torrentInfo := b.GetTorrentInfo(b.getTorrentInfoHash(handle))
	for i := 0; i < len(torrentInfo.Files); i++ {
		torrentInfo.Files[i].SetInitialPriority()
	}

	log.Printf("[scrapmagnet] Metadata received %v", handle.Status().GetName())
	trackingEvent("Metadata received", map[string]interface{}{"Magnet InfoHash": b.getTorrentInfoHash(handle), "Magnet Name": handle.Status().GetName()}, b.mixpanelData[b.getTorrentInfoHash(handle)])
}

func (b *BitTorrent) onTorrentPaused(handle libtorrent.Torrent_handle) {
	if !b.connectionInfos[b.getTorrentInfoHash(handle)].paused {
		log.Printf("[scrapmagnet] Paused %v", handle.Status().GetName())
		b.connectionInfos[b.getTorrentInfoHash(handle)].paused = true
	}
}

func (b *BitTorrent) onTorrentResumed(handle libtorrent.Torrent_handle) {
	if b.connectionInfos[b.getTorrentInfoHash(handle)].paused {
		log.Printf("[scrapmagnet] Resumed %v", handle.Status().GetName())
		b.connectionInfos[b.getTorrentInfoHash(handle)].paused = false
	}
}

func (b *BitTorrent) onTorrentFinished(handle libtorrent.Torrent_handle) {
	log.Printf("[scrapmagnet] Finished %v", handle.Status().GetName())
	trackingEvent("Finished", map[string]interface{}{"Magnet InfoHash": b.getTorrentInfoHash(handle), "Magnet Name": handle.Status().GetName()}, b.mixpanelData[b.getTorrentInfoHash(handle)])
}

func (b *BitTorrent) onTorrentRemoved(handle libtorrent.Torrent_handle) {
	log.Printf("[scrapmagnet] Removed %v", handle.Status().GetName())
	trackingEvent("Removed", map[string]interface{}{"Magnet InfoHash": b.getTorrentInfoHash(handle), "Magnet Name": handle.Status().GetName()}, b.mixpanelData[b.getTorrentInfoHash(handle)])

	delete(b.mixpanelData, b.getTorrentInfoHash(handle))
	delete(b.lookAhead, b.getTorrentInfoHash(handle))
	delete(b.connectionInfos, b.getTorrentInfoHash(handle))
	b.removeChan <- true
}

func (b *BitTorrent) onTorrentDeleted(infoHash string, success bool) {
	b.deleteChan <- success

	{
		if success {
			log.Printf("[scrapmagnet] Deleted")
		} else {
			log.Printf("[scrapmagnet] Delete failed")
		}
	}
}
