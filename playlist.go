package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Playlist represents a user's playlist
type Playlist struct {
	Name  string      `json:"name"`
	Songs []MusicItem `json:"songs"`
}

// PlaylistManager manages all playlists
type PlaylistManager struct {
	mu            sync.RWMutex
	Playlists     map[string]*Playlist      // Legacy playlists (backward compatible)
	UserPlaylists map[string][]*UserPlaylist // key: user ID, value: user's playlists
	filePath      string
	userFilePath  string
}

var playlistManager *PlaylistManager

// prependSong 把歌曲插到列表最前面（新收进的歌排第一）。
// 用原地搬移而不是 append([]MusicItem{song}, list...)，
// 后者每加一首都要重新分配整块内存，几百首的歌单会明显变慢。
func prependSong(list []MusicItem, song MusicItem) []MusicItem {
	list = append(list, MusicItem{})
	copy(list[1:], list[:len(list)-1])
	list[0] = song
	return list
}

// InitPlaylistManager initializes the playlist manager
func InitPlaylistManager() {
	playlistManager = &PlaylistManager{
		Playlists:     make(map[string]*Playlist),
		UserPlaylists: make(map[string][]*UserPlaylist),
		filePath:      "./files/playlists.json",
		userFilePath:  "./files/user_playlists.json",
	}
	
	// Create files directory if it doesn't exist
	os.MkdirAll("./files", 0755)
	
	// Load existing playlists (backward compatible)
	playlistManager.loadFromFile()
	
	// Load user playlists
	playlistManager.loadUserPlaylists()
	
	// Initialize "我喜欢" playlist if it doesn't exist (backward compatible)
	if _, exists := playlistManager.Playlists["favorite"]; !exists {
		playlistManager.Playlists["favorite"] = &Playlist{
			Name:  "我喜欢",
			Songs: []MusicItem{},
		}
		playlistManager.saveToFile()
	}
}

// loadFromFile loads playlists from JSON file
func (pm *PlaylistManager) loadFromFile() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	data, err := os.ReadFile(pm.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist yet, that's ok
		}
		return err
	}

	return json.Unmarshal(data, &pm.Playlists)
}

// saveToFile saves playlists to JSON file
// NOTE: Caller must hold the lock!
func (pm *PlaylistManager) saveToFile() error {
	data, err := json.MarshalIndent(pm.Playlists, "", "  ")
	if err != nil {
		return err
	}

	return writeFileAtomic(pm.filePath, data, 0o600)
}

// AddToPlaylist adds a song to a playlist
func (pm *PlaylistManager) AddToPlaylist(playlistName string, song MusicItem) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlist, exists := pm.Playlists[playlistName]
	if !exists {
		playlist = &Playlist{
			Name:  playlistName,
			Songs: []MusicItem{},
		}
		pm.Playlists[playlistName] = playlist
	}

	// Check if song already exists in playlist
	for _, s := range playlist.Songs {
		if s.Title == song.Title && s.Artist == song.Artist {
			return fmt.Errorf("song already exists in playlist")
		}
	}

	// 最新收进的歌曲排到最前面
	playlist.Songs = prependSong(playlist.Songs, song)
	return pm.saveToFile()
}

// RemoveFromPlaylist removes a song from a playlist
func (pm *PlaylistManager) RemoveFromPlaylist(playlistName string, title, artist string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlist, exists := pm.Playlists[playlistName]
	if !exists {
		return fmt.Errorf("playlist not found")
	}

	for i, song := range playlist.Songs {
		if song.Title == title && song.Artist == artist {
			playlist.Songs = append(playlist.Songs[:i], playlist.Songs[i+1:]...)
			return pm.saveToFile()
		}
	}

	return fmt.Errorf("song not found in playlist")
}

// GetPlaylist returns a playlist
func (pm *PlaylistManager) GetPlaylist(playlistName string) (*Playlist, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	playlist, exists := pm.Playlists[playlistName]
	if !exists {
		return nil, fmt.Errorf("playlist not found")
	}

	return playlist, nil
}

// HTTP Handlers

// HandleAddToFavorite handles adding a song to favorites (user-specific)
func HandleAddToFavorite(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[API] Add to favorite request received from %s\n", r.RemoteAddr)
	
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var userID string
	
	// First, check for ESP32 device token (X-Device-Token header)
	deviceToken := r.Header.Get("X-Device-Token")
	if deviceToken != "" {
		dm := GetDeviceManager()
		device, err := dm.VerifyToken(deviceToken)
		if err == nil {
			userID = userStore.GetUserIDByUsername(device.Username)
			fmt.Printf("[API] ESP32 device adding favorite (user: %s, userID: %s)\n", device.Username, userID)
		}
	}
	
	// If not ESP32, check for web user token (Authorization header)
	if userID == "" {
		token := r.Header.Get("Authorization")
		if token != "" {
			token = strings.TrimPrefix(token, "Bearer ")
			user, err := userStore.GetUserByToken(token)
			if err == nil {
				userID = user.ID
			}
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		fmt.Printf("[API] Error reading body: %v\n", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	
	fmt.Printf("[API] Received body: %s\n", string(body))

	var song MusicItem
	err = json.Unmarshal(body, &song)
	if err != nil {
		fmt.Printf("[API] Error parsing JSON: %v\n", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	fmt.Printf("[API] Adding to favorites: %s - %s (userID: %s)\n", song.Artist, song.Title, userID)

	// If user is authenticated, add to user's "我喜欢" playlist
	if userID != "" {
		// Get user's "我喜欢" playlist (first playlist)
		playlists := playlistManager.GetUserPlaylists(userID)
		
		// If user has no playlists, initialize them first
		if len(playlists) == 0 {
			fmt.Printf("[API] User %s has no playlists, initializing...\n", userID)
			playlistManager.InitializeUserPlaylists(userID)
			playlists = playlistManager.GetUserPlaylists(userID)
		}
		
		if len(playlists) > 0 {
			favoritePlaylist := playlists[0] // "我喜欢" is always the first playlist
			err = playlistManager.AddSongToUserPlaylist(userID, favoritePlaylist.ID, song)
			if err != nil {
				if err.Error() == "song already exists in playlist" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]string{
						"status":  "success",
						"message": "歌曲已在收藏列表中",
					})
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			// Playlist initialization failed
			fmt.Printf("[Error] Failed to initialize playlists for user %s\n", userID)
			http.Error(w, "Failed to initialize user playlists", http.StatusInternalServerError)
			return
		}
	} else {
		// Fallback to global favorite playlist for anonymous users
		err = playlistManager.AddToPlaylist("favorite", song)
		if err != nil {
			if err.Error() == "song already exists in playlist" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{
					"status":  "success",
					"message": "歌曲已在收藏列表中",
				})
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	fmt.Printf("[Info] Added to favorites: %s - %s\n", song.Artist, song.Title)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "收藏成功",
	})
}

// HandleRemoveFromFavorite handles removing a song from favorites (user-specific)
func HandleRemoveFromFavorite(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[API] Remove from favorite request received from %s\n", r.RemoteAddr)
	
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var userID string
	
	// First, check for ESP32 device token (X-Device-Token header)
	deviceToken := r.Header.Get("X-Device-Token")
	if deviceToken != "" {
		dm := GetDeviceManager()
		device, err := dm.VerifyToken(deviceToken)
		if err == nil {
			userID = userStore.GetUserIDByUsername(device.Username)
			fmt.Printf("[API] ESP32 device removing favorite (user: %s, userID: %s)\n", device.Username, userID)
		}
	}
	
	// If not ESP32, check for web user token (Authorization header)
	if userID == "" {
		token := r.Header.Get("Authorization")
		if token != "" {
			token = strings.TrimPrefix(token, "Bearer ")
			user, err := userStore.GetUserByToken(token)
			if err == nil {
				userID = user.ID
			}
		}
	}

	// Read song info from body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	
	var song MusicItem
	err = json.Unmarshal(body, &song)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	title := song.Title
	artist := song.Artist

	if title == "" || artist == "" {
		http.Error(w, "Missing title or artist", http.StatusBadRequest)
		return
	}
	
	fmt.Printf("[API] Removing from favorites: %s - %s (userID: %s)\n", artist, title, userID)

	// If user is authenticated, remove from user's "我喜欢" playlist
	if userID != "" {
		playlists := playlistManager.GetUserPlaylists(userID)
		
		// If user has no playlists, nothing to remove
		if len(playlists) == 0 {
			fmt.Printf("[API] User %s has no playlists, nothing to remove\n", userID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "success",
				"message": "歌曲不在收藏列表中",
			})
			return
		}
		
		favoritePlaylist := playlists[0]
		err = playlistManager.RemoveSongFromUserPlaylist(userID, favoritePlaylist.ID, title, artist)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		// Fallback to global favorite playlist
		err = playlistManager.RemoveFromPlaylist("favorite", title, artist)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	fmt.Printf("[Info] Removed from favorites: %s - %s\n", artist, title)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "取消收藏成功",
	})
}

// HandleGetFavorites handles getting the favorite playlist (user-specific)
func HandleGetFavorites(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[API] Get favorites request received from %s\n", r.RemoteAddr)
	
	var userID string
	
	// First, check for ESP32 device token (X-Device-Token header)
	deviceToken := r.Header.Get("X-Device-Token")
	if deviceToken != "" {
		dm := GetDeviceManager()
		device, err := dm.VerifyToken(deviceToken)
		if err == nil {
			// Device.Username is the username, need to convert to userID
			userID = userStore.GetUserIDByUsername(device.Username)
			fmt.Printf("[API] ESP32 device authenticated: %s (user: %s, userID: %s)\n", device.MAC, device.Username, userID)
		} else {
			fmt.Printf("[API] Invalid device token: %v\n", err)
		}
	}
	
	// If not ESP32, check for web user token (Authorization header)
	if userID == "" {
		token := r.Header.Get("Authorization")
		if token != "" {
			token = strings.TrimPrefix(token, "Bearer ")
			user, err := userStore.GetUserByToken(token)
			if err == nil {
				userID = user.ID
				fmt.Printf("[API] Web user authenticated: %s\n", userID)
			}
		}
	}

	// If user is authenticated, return user's "我喜欢" playlist
	if userID != "" {
		playlists := playlistManager.GetUserPlaylists(userID)
		
		// If user has no playlists, initialize them first
		if len(playlists) == 0 {
			fmt.Printf("[API] User %s has no playlists, initializing...\n", userID)
			playlistManager.InitializeUserPlaylists(userID)
			playlists = playlistManager.GetUserPlaylists(userID)
		}
		
		if len(playlists) > 0 {
			favoritePlaylist := playlists[0] // "我喜欢" is always the first playlist
			fmt.Printf("[API] Returning user %s favorites: %d songs\n", userID, len(favoritePlaylist.Songs))

			// 与 /api/user/playlist/songs 一致：带 offset/limit 时分页返回，
			// 老固件不传参数仍拿纯数组（收藏也可能很长，同样会撑爆设备解析内存）。
			offset, limit, paged := parsePageParams(r)
			if paged {
				total := len(favoritePlaylist.Songs)
				start := offset
				if start > total {
					start = total
				}
				end := start + limit
				if end > total {
					end = total
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"total":  total,
					"offset": start,
					"limit":  limit,
					"songs":  favoritePlaylist.Songs[start:end],
				})
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Return just the songs array for frontend compatibility
			json.NewEncoder(w).Encode(favoritePlaylist.Songs)
			return
		}
		
		// If still no playlists (initialization failed), return empty array
		fmt.Printf("[API] User %s playlists initialization failed, returning empty\n", userID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]MusicItem{})
		return
	}
	
	// Only return global favorite playlist if user is NOT authenticated
	fmt.Printf("[API] No authentication found, returning global favorites\n")
	playlist, err := playlistManager.GetPlaylist("favorite")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]MusicItem{}) // Return empty array
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(playlist.Songs)
}

// resolveUserID 解析当前请求所属用户（设备 token / 登录 token / 会话 cookie）。
// 未登录返回空字符串。
func resolveUserID(r *http.Request) string {
	if deviceToken := r.Header.Get("X-Device-Token"); deviceToken != "" {
		if device, err := GetDeviceManager().VerifyToken(deviceToken); err == nil {
			if id := userStore.GetUserIDByUsername(device.Username); id != "" {
				return id
			}
		}
	}
	if token := r.Header.Get("Authorization"); token != "" {
		token = strings.TrimPrefix(token, "Bearer ")
		if user, err := userStore.GetUserByToken(token); err == nil {
			return user.ID
		}
	}
	if cookie, err := r.Cookie("session_token"); err == nil {
		if user, err := userStore.GetUserByToken(cookie.Value); err == nil {
			return user.ID
		}
	}
	return ""
}

func songInList(list []MusicItem, title, artist string) bool {
	for _, song := range list {
		if song.Title == title && song.Artist == artist {
			return true
		}
	}
	return false
}

// HandleCheckFavorite checks if a song is in favorites
//
// 旧实现只查全局 favorite 歌单，但登录用户的收藏其实存在自己的
// 「我喜欢」里，导致 Web 端「已收藏」永远显示未收藏。
func HandleCheckFavorite(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	artist := r.URL.Query().Get("artist")

	if title == "" || artist == "" {
		http.Error(w, "Missing title or artist", http.StatusBadRequest)
		return
	}

	isFavorite := false

	if userID := resolveUserID(r); userID != "" {
		playlists := playlistManager.GetUserPlaylists(userID)
		if len(playlists) > 0 && songInList(playlists[0].Songs, title, artist) {
			isFavorite = true
		}
	} else if playlist, err := playlistManager.GetPlaylist("favorite"); err == nil {
		isFavorite = songInList(playlist.Songs, title, artist)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"is_favorite": isFavorite})
}

// User Playlist Management Functions

// loadUserPlaylists loads user playlists from JSON file
func (pm *PlaylistManager) loadUserPlaylists() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	data, err := os.ReadFile(pm.userFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist yet, that's ok
		}
		return err
	}

	return json.Unmarshal(data, &pm.UserPlaylists)
}

// saveUserPlaylists saves user playlists to JSON file
// NOTE: Caller must hold the lock!
func (pm *PlaylistManager) saveUserPlaylists() error {
	data, err := json.MarshalIndent(pm.UserPlaylists, "", "  ")
	if err != nil {
		return err
	}

	return writeFileAtomic(pm.userFilePath, data, 0o600)
}

// InitializeUserPlaylists creates default playlists for a new user
func (pm *PlaylistManager) InitializeUserPlaylists(userID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.UserPlaylists[userID]; exists {
		return // Already initialized
	}

	// Create "我喜欢" playlist for the user
	favoritePlaylist := &UserPlaylist{
		ID:          generateID(),
		UserID:      userID,
		Name:        "我喜欢",
		Description: "我喜欢的音乐",
		Songs:       []MusicItem{},
		IsPublic:    false,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	pm.UserPlaylists[userID] = []*UserPlaylist{favoritePlaylist}
	pm.saveUserPlaylists()
}

// CreateUserPlaylist creates a new playlist for a user
func (pm *PlaylistManager) CreateUserPlaylist(userID, name, description string) (*UserPlaylist, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlist := &UserPlaylist{
		ID:          generateID(),
		UserID:      userID,
		Name:        name,
		Description: description,
		Songs:       []MusicItem{},
		IsPublic:    false,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	pm.UserPlaylists[userID] = append(pm.UserPlaylists[userID], playlist)
	pm.saveUserPlaylists()

	return playlist, nil
}

// GetUserPlaylists returns all playlists for a user
func (pm *PlaylistManager) GetUserPlaylists(userID string) []*UserPlaylist {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.UserPlaylists[userID]
}

// GetUserPlaylistByID returns a specific playlist
func (pm *PlaylistManager) GetUserPlaylistByID(userID, playlistID string) (*UserPlaylist, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	playlists, exists := pm.UserPlaylists[userID]
	if !exists {
		return nil, fmt.Errorf("no playlists found for user")
	}

	for _, playlist := range playlists {
		if playlist.ID == playlistID {
			return playlist, nil
		}
	}

	return nil, fmt.Errorf("playlist not found")
}

// AddSongToUserPlaylist adds a song to a user's playlist
func (pm *PlaylistManager) AddSongToUserPlaylist(userID, playlistID string, song MusicItem) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlists, exists := pm.UserPlaylists[userID]
	if !exists {
		return fmt.Errorf("no playlists found for user")
	}

	for _, playlist := range playlists {
		if playlist.ID == playlistID {
			// Check if song already exists
			for _, s := range playlist.Songs {
				if s.Title == song.Title && s.Artist == song.Artist {
					return fmt.Errorf("song already exists in playlist")
				}
			}

			// 最新收进的歌曲排到最前面
			playlist.Songs = prependSong(playlist.Songs, song)
			playlist.UpdatedAt = time.Now()
			return pm.saveUserPlaylists()
		}
	}

	return fmt.Errorf("playlist not found")
}

// AddSongsToUserPlaylist 批量添加（歌单导入用）。
// 逐条调 AddSongToUserPlaylist 会每首都写一次文件（100 首 = 100 次 fsync），
// 这里一次加锁、一次落盘，并保持导入顺序。
func (pm *PlaylistManager) AddSongsToUserPlaylist(userID, playlistID string, songs []MusicItem) (added, skipped int, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlists, exists := pm.UserPlaylists[userID]
	if !exists {
		return 0, 0, fmt.Errorf("no playlists found for user")
	}

	for _, playlist := range playlists {
		if playlist.ID != playlistID {
			continue
		}

		seen := make(map[string]bool, len(playlist.Songs)+len(songs))
		for _, s := range playlist.Songs {
			seen[musicDedupeKey(s.Title, s.Artist)] = true
		}
		for _, s := range songs {
			key := musicDedupeKey(s.Title, s.Artist)
			if seen[key] {
				skipped++
				continue
			}
			seen[key] = true
			playlist.Songs = append(playlist.Songs, s)
			added++
		}
		playlist.UpdatedAt = time.Now()
		return added, skipped, pm.saveUserPlaylists()
	}

	return 0, 0, fmt.Errorf("playlist not found")
}

// musicDedupeKey 判重用的键：歌名 + 歌手，忽略大小写与首尾空格。
func musicDedupeKey(title, artist string) string {
	return toLower(strings.TrimSpace(title)) + "\x00" + toLower(strings.TrimSpace(artist))
}

// RemoveSongFromUserPlaylist removes a song from a user's playlist
func (pm *PlaylistManager) RemoveSongFromUserPlaylist(userID, playlistID, title, artist string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlists, exists := pm.UserPlaylists[userID]
	if !exists {
		return fmt.Errorf("no playlists found for user")
	}

	for _, playlist := range playlists {
		if playlist.ID == playlistID {
			for i, song := range playlist.Songs {
				if song.Title == title && song.Artist == artist {
					playlist.Songs = append(playlist.Songs[:i], playlist.Songs[i+1:]...)
					playlist.UpdatedAt = time.Now()
					return pm.saveUserPlaylists()
				}
			}
			return fmt.Errorf("song not found in playlist")
		}
	}

	return fmt.Errorf("playlist not found")
}

// ClearUserPlaylistSongs 清空一个歌单里的全部歌曲，歌单本身保留。
// 返回被清掉的歌曲数量。「我喜欢」是受保护歌单，不允许整体清空，
// 避免一键把所有收藏抹掉（要删单曲仍可逐首删）。
func (pm *PlaylistManager) ClearUserPlaylistSongs(userID, playlistID string) (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlists, exists := pm.UserPlaylists[userID]
	if !exists {
		return 0, fmt.Errorf("no playlists found for user")
	}

	for i, playlist := range playlists {
		if playlist.ID != playlistID {
			continue
		}
		if playlist.Name == "我喜欢" && i == 0 {
			return 0, fmt.Errorf("cannot clear favorite playlist")
		}
		removed := len(playlist.Songs)
		playlist.Songs = []MusicItem{}
		playlist.UpdatedAt = time.Now()
		return removed, pm.saveUserPlaylists()
	}

	return 0, fmt.Errorf("playlist not found")
}

// DeleteUserPlaylist deletes a user's playlist
func (pm *PlaylistManager) DeleteUserPlaylist(userID, playlistID string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	playlists, exists := pm.UserPlaylists[userID]
	if !exists {
		return fmt.Errorf("no playlists found for user")
	}

	for i, playlist := range playlists {
		if playlist.ID == playlistID {
			// Don't allow deleting "我喜欢" playlist
			if playlist.Name == "我喜欢" && i == 0 {
				return fmt.Errorf("cannot delete favorite playlist")
			}

			pm.UserPlaylists[userID] = append(playlists[:i], playlists[i+1:]...)
			return pm.saveUserPlaylists()
		}
	}

	return fmt.Errorf("playlist not found")
}

// User Playlist HTTP Handlers

// HandleCreateUserPlaylist handles creating a new playlist
func HandleCreateUserPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "Playlist name is required", http.StatusBadRequest)
		return
	}

	playlist, err := playlistManager.CreateUserPlaylist(userID, req.Name, req.Description)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("[Info] User %s created playlist: %s\n", userID, req.Name)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(playlist)
}

// HandleGetUserPlaylists handles getting all user playlists
func HandleGetUserPlaylists(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	playlists := playlistManager.GetUserPlaylists(userID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(playlists)
}

// HandleAddSongToUserPlaylist handles adding a song to a playlist
func HandleAddSongToUserPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	playlistID := r.URL.Query().Get("playlist_id")
	if playlistID == "" {
		http.Error(w, "Missing playlist_id parameter", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var song MusicItem
	if err := json.Unmarshal(body, &song); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	err = playlistManager.AddSongToUserPlaylist(userID, playlistID, song)
	if err != nil {
		if err.Error() == "song already exists in playlist" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "success",
				"message": "歌曲已在歌单中",
			})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("[Info] User %s added song to playlist %s: %s - %s\n", userID, playlistID, song.Artist, song.Title)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "添加成功",
	})
}

// HandleRemoveSongFromUserPlaylist handles removing a song from a playlist
func HandleRemoveSongFromUserPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	playlistID := r.URL.Query().Get("playlist_id")
	title := r.URL.Query().Get("title")
	artist := r.URL.Query().Get("artist")

	if playlistID == "" || title == "" || artist == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		return
	}

	err := playlistManager.RemoveSongFromUserPlaylist(userID, playlistID, title, artist)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("[Info] User %s removed song from playlist %s: %s - %s\n", userID, playlistID, artist, title)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "移除成功",
	})
}

// HandleClearUserPlaylistSongs 一键清空歌单内的全部歌曲（歌单保留）
func HandleClearUserPlaylistSongs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	playlistID := r.URL.Query().Get("playlist_id")
	if playlistID == "" {
		http.Error(w, "Missing playlist_id parameter", http.StatusBadRequest)
		return
	}

	removed, err := playlistManager.ClearUserPlaylistSongs(userID, playlistID)
	if err != nil {
		if err.Error() == "cannot clear favorite playlist" {
			http.Error(w, "「我喜欢」不能一键清空，请逐首取消喜欢", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Printf("[Info] User %s cleared playlist %s (%d songs)\n", userID, playlistID, removed)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"removed": removed,
		"message": fmt.Sprintf("已清空 %d 首", removed),
	})
}

// HandleDeleteUserPlaylist handles deleting a playlist
func HandleDeleteUserPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	playlistID := r.URL.Query().Get("playlist_id")
	if playlistID == "" {
		http.Error(w, "Missing playlist_id parameter", http.StatusBadRequest)
		return
	}

	err := playlistManager.DeleteUserPlaylist(userID, playlistID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Printf("[Info] User %s deleted playlist %s\n", userID, playlistID)

	// 歌单没了，挂在它上面的自动同步关系也一起清掉，免得每天报「歌单不存在」
	removeJobsForPlaylist(userID, playlistID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "删除成功",
	})
}

// 设备侧分页参数
const (
	playlistPageDefault = 50  // 不传 limit 时的每页条数
	playlistPageMax     = 200 // 单次最多下发条数，防止被滥用拉爆设备
)

// parsePageParams 解析 offset/limit 分页参数。
// paged=false 表示调用方没要分页（老固件），此时应保持原来的"纯数组 + 单次上限"行为。
func parsePageParams(r *http.Request) (offset, limit int, paged bool) {
	q := r.URL.Query()
	if q.Get("offset") == "" && q.Get("limit") == "" {
		return 0, 0, false
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	limit = playlistPageDefault
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > playlistPageMax {
		limit = playlistPageMax
	}
	return offset, limit, true
}

// HandleGetPlaylistSongs 按歌单名返回精简歌曲列表，专供 ESP32 播放使用。
// 只返回 title/artist 两个字段，避免把每首歌的 5 个长 URL 一起序列化，
// 否则 60 首歌的全量 JSON 约 30KB，ESP32 同步拉取+解析会饿死音频任务导致卡死。
// 鉴权由 AuthMiddleware 完成，userID 从 X-User-ID 读取（X-Device-Token 会被自动转换）。
func HandleGetPlaylistSongs(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 优先从 query 取；为空则从 POST JSON body 取（ESP32 用 POST 传中文歌单名，避免URL编码）
	name := r.URL.Query().Get("name")
	if name == "" && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		body, err := io.ReadAll(r.Body)
		if err == nil && len(body) > 0 {
			var req struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(body, &req) == nil {
				name = req.Name
			}
		}
		defer r.Body.Close()
	}
	playlists := playlistManager.GetUserPlaylists(userID)
	if len(playlists) == 0 {
		json.NewEncoder(w).Encode([]map[string]string{})
		return
	}

	// 精简结构：只有歌名和歌手
	type slimSong struct {
		Title  string `json:"title"`
		Artist string `json:"artist"`
	}

	// 找同名歌单
	var target *UserPlaylist
	if name == "我喜欢" || name == "我喜欢的" || name == "收藏" || name == "favorite" {
		target = playlists[0] // "我喜欢" 固定是第一个
	} else {
		for _, p := range playlists {
			if p.Name == name {
				target = p
				break
			}
		}
	}

	// ESP32 老固件会把整包 JSON 一次性解析成 vector<SongInfo>，条数太多时内存不足，
	// 表现为「歌单拉下来了但一个音频请求都不发、完全没声音」（198 首 ≈ 9KB 实测复现）。
	// maxSongs 只对不分页的老固件生效；新固件用 offset/limit 分页，逐页拉全量。
	maxSongs := playlistSongsMaxForDevice()
	offset, limit, paged := parsePageParams(r)

	result := []slimSong{}
	total := 0
	start, end := 0, 0
	if target != nil {
		total = len(target.Songs)
		end = total
		if paged {
			start = offset
			if start > total {
				start = total
			}
			end = start + limit
			if end > total {
				end = total
			}
		} else if maxSongs > 0 && end > maxSongs {
			end = maxSongs
		}
		for _, s := range target.Songs[start:end] {
			result = append(result, slimSong{Title: s.Title, Artist: s.Artist})
		}
	}

	if paged {
		fmt.Printf("[API] GetPlaylistSongs user=%s name=%s -> page offset=%d limit=%d, %d/%d songs\n",
			userID, name, start, limit, len(result), total)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total":  total,
			"offset": start,
			"limit":  limit,
			"songs":  result, // 空时是 []，不是 null
		})
		return
	}

	fmt.Printf("[API] GetPlaylistSongs user=%s name=%s -> %d/%d songs (legacy limit=%d)\n",
		userID, name, len(result), total, maxSongs)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// playlistSongsMaxForDevice 返回下发给设备的最大歌曲数。
// 设备端内存吃紧，一次给太多会解析失败导致整个歌单没声音。
func playlistSongsMaxForDevice() int {
	// 实测（ESP32-S3，Beyond 歌单 198 首）：60 / 100 / 150 均可正常起播，
	// 198 首拉到后一声不响。取 150 作为默认值，留一点余量。
	const defaultMax = 150
	if v := os.Getenv("PLAYLIST_SONGS_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMax
}

