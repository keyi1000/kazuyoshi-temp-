package matchmaking

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sys3/api/rate"
	"time"

	"github.com/gorilla/websocket"
)

var (
	rooms      = make(map[string]*Room)
	roomsMutex sync.Mutex
	upgrader   = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // 全てのオリジンを許可
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	db *sql.DB
)

// WebSocketを使用したマッチメイキングハンドラー
func MatchmakingHandler(w http.ResponseWriter, r *http.Request) {

	// WebSocket接続のアップグレード
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("WebSocketアップグレードエラー: %v\n", err)
		http.Error(w, fmt.Sprintf("WebSocketアップグレード失敗: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// Cookieの確認
	cookies := r.Cookies()
	fmt.Printf("受け取ったクッキー: %+v\n", cookies)

	cookie, err := r.Cookie("username")
	if err != nil {
		fmt.Printf("クッキーエラー: %v\n", err)
		conn.WriteJSON(map[string]string{
			"status":  "unauthorized",
			"message": "ログインが必要です",
		})
		return
	}

	fmt.Printf("見つかったユーザー名クッキー: %+v\n", cookie)

	fmt.Printf("WebSocket接続確立: %s\n", cookie.Value)

	roomsMutex.Lock()

	// 空いている部屋を探す
	var matchedRoom *Room
	for _, room := range rooms {
		if room.PlayerID != cookie.Value && !room.IsMatched {
			matchedRoom = room
			matchedRoom.IsMatched = false
			matchedRoom.Player2ID = cookie.Value
			break
		}
	}

	if matchedRoom != nil {
		// 既存の部屋とマッチングが成功した場合の処理
		matchedRoom.IsMatched = true
		matchedRoom.Player2ID = cookie.Value
		matchedRoom.Player2Conn = conn
		roomsMutex.Unlock()

		// 両プレイヤーにマッチング成功を通知
		matchResponse := map[string]string{
			"status":  "matched",
			"room_id": matchedRoom.ID,
		}
		matchedRoom.Player1Conn.WriteJSON(matchResponse)
		conn.WriteJSON(matchResponse)

		// 接続を維持
		select {}

		// Player1の場合のみゲームセッションを開始
		handleGameSession(matchedRoom)
		return
	}

	// マッチする部屋が見つからなかった場合、新しい部屋を作成
	newRoom := &Room{
		ID:          generateRoomID(),
		PlayerID:    cookie.Value,
		Player1Conn: conn,
		CreatedAt:   time.Now(),
		IsMatched:   false,
	}
	rooms[newRoom.ID] = newRoom
	roomsMutex.Unlock()

	// クライアントに待機状態を通知
	conn.WriteJSON(map[string]string{
		"status":  "waiting",
		"room_id": newRoom.ID,
	})

	// マッチングを待機
	if waitForMatch(newRoom) {
		// 部屋作成者（Player1）の場合のみゲームセッションを開始
		handleGameSession(newRoom)
	}
	// マッチングがタイムアウトした場合は、この時点で処理が終了する
}

func generateRoomID() string {
	// ユニークな部屋IDを生成する実装
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func handleGameSession(room *Room) {
	// 出題済みの問題IDを管理
	usedQuestionIDs := make(map[int]bool)

	// 利用可能な問題の総数を取得
	var totalQuestions int
	err := db.QueryRow("SELECT COUNT(*) FROM questions").Scan(&totalQuestions)
	if err != nil {
		log.Printf("問題数取得エラー: %v", err)
		return
	}

	// ゲーム開始メッセージを送信
	startMessage := map[string]string{
		"status":  "game_start",
		"message": "対戦を開始します",
	}
	if err := room.Player1Conn.WriteJSON(startMessage); err != nil {
		log.Printf("Player1へのゲーム開始メッセージ送信エラー: %v", err)
		return
	}
	if err := room.Player2Conn.WriteJSON(startMessage); err != nil {
		log.Printf("Player2へのゲーム開始メッセージ送信エラー: %v", err)
		return
	}

	// スコアを管理
	player1Score := 0
	player2Score := 0

	// 問題数を管理（利用可能な問題数と5問のうち少ない方）
	const number_of_questions = 5 // ここで問題数を指定できつ
	questionsPerGame := min(number_of_questions, totalQuestions)

	for questionCount := 0; questionCount < questionsPerGame; questionCount++ {
		// まだ出題していない問題を取得
		var question Question
		for {
			err := db.QueryRow(`
				SELECT id, question_text, correct_answer, choice1, choice2, choice3, choice4 
				FROM questions 
				ORDER BY RAND() 
				LIMIT 1
			`).Scan(
				&question.ID,
				&question.QuestionText,
				&question.CorrectAnswer,
				&question.Choices[0],
				&question.Choices[1],
				&question.Choices[2],
				&question.Choices[3],
			)
			if err != nil {
				log.Printf("問題取得エラー: %v", err)
				return
			}

			// 未出題の問題であれば使用
			if !usedQuestionIDs[question.ID] {
				usedQuestionIDs[question.ID] = true
				break
			}
		}

		// 問題を送信
		questionMessage := map[string]interface{}{
			"status":   "question",
			"question": question,
		}

		// 両プレイヤーに順番に送信
		if err := room.Player1Conn.WriteJSON(questionMessage); err != nil {
			log.Printf("Player1への問題送信エラー: %v", err)
			return
		}
		if err := room.Player2Conn.WriteJSON(questionMessage); err != nil {
			log.Printf("Player2への問題送信エラー: %v", err)
			return
		}

		// 問題送信後、少し待機
		time.Sleep(1 * time.Second)

		// 回答権管理用のチャネル
		answerRights := make(chan string, 1)
		answerTimeout := time.After(10 * time.Second)
		var answered bool

		// 両プレイヤーからの回答リクエストを待機
		go handleAnswerRequest(room.Player1Conn, room.PlayerID, answerRights)
		go handleAnswerRequest(room.Player2Conn, room.Player2ID, answerRights)

		// 回答権または制限時間待ち
		select {
		case playerID := <-answerRights:
			// 回答権獲得を両プレイヤーに通知
			rightsGrantedMessage := map[string]interface{}{
				"status":    "answer_rights_granted",
				"message":   "回答権が獲得されました",
				"player_id": playerID, // どのプレイヤーが回答権を得たか
			}

			// 両プレイヤーに通知を送信
			if err := room.Player1Conn.WriteJSON(rightsGrantedMessage); err != nil {
				log.Printf("Player1への回答権通知エラー: %v", err)
			}
			if err := room.Player2Conn.WriteJSON(rightsGrantedMessage); err != nil {
				log.Printf("Player2への回答権通知エラー: %v", err)
			}

			// 回答権を得たプレイヤーの回答を待機
			answered = handlePlayerAnswer(room, playerID, question.CorrectAnswer)

			// スコアの更新
			if answered {
				if playerID == room.PlayerID {
					player1Score++
				} else {
					player2Score++
				}

				// スコア更新を両プレイヤーに通知
				scoreMessage := map[string]interface{}{
					"status":        "score_update",
					"player1_score": player1Score,
					"player2_score": player2Score,
				}
				room.Player1Conn.WriteJSON(scoreMessage)
				room.Player2Conn.WriteJSON(scoreMessage)
			}

		case <-answerTimeout:
			// 制限時間切れ
			timeoutMessage := map[string]string{
				"status":  "timeout",
				"message": "制限時間切れ",
			}
			room.Player1Conn.WriteJSON(timeoutMessage)
			room.Player2Conn.WriteJSON(timeoutMessage)
		}

		// 次の問題までの待機時間
		time.Sleep(3 * time.Second)
	}

	// 最終結果の通知
	finalResult := map[string]interface{}{
		"status": "game_end",
		"final_scores": map[string]interface{}{
			"player1": map[string]interface{}{
				"id":    room.PlayerID,
				"score": player1Score,
			},
			"player2": map[string]interface{}{
				"id":    room.Player2ID,
				"score": player2Score,
			},
		},
		"winner": determineWinner(room.PlayerID, room.Player2ID, player1Score, player2Score),
	}

	room.Player1Conn.WriteJSON(finalResult)
	room.Player2Conn.WriteJSON(finalResult)

	// レート計算と更新
	updatePlayerRatings(db, finalResult["winner"].(map[string]string)["id"],
		finalResult["winner"].(map[string]string)["loser_id"])
}

func handleAnswerRequest(conn *websocket.Conn, playerID string, answerRights chan<- string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("handleAnswerRequest でパニック発生: %v", r)
		}
	}()

	for {
		var message map[string]interface{}
		err := conn.ReadJSON(&message)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("予期せぬ接続切断: %v", err)
			} else {
				log.Printf("メッセージ読み取りエラー: %v", err)
			}
			return
		}

		log.Printf("受信したメッセージ: %+v", message)

		if message["type"] == "answer_request" {
			select {
			case answerRights <- playerID:
				log.Printf("プレイヤー %s が回答権を獲得", playerID)
				// 回答権獲得の通知は handleGameSession で行うため、ここでは即座に return
				return
			default:
				// 他のプレイヤーが既に回答権を取得している
				err := conn.WriteJSON(map[string]string{
					"status":  "answer_denied",
					"message": "他のプレイヤーが回答中です",
				})
				if err != nil {
					log.Printf("回答権拒否メッセージ送信エラー: %v", err)
					return
				}
			}
		}
	}
}

func handlePlayerAnswer(room *Room, playerID string, correctAnswer string) bool {
	log.Printf("プレイヤー %s の回答を待機中", playerID)

	var conn *websocket.Conn
	var otherConn *websocket.Conn

	if playerID == room.PlayerID {
		conn = room.Player1Conn
		otherConn = room.Player2Conn
	} else {
		conn = room.Player2Conn
		otherConn = room.Player1Conn
	}

	// 回答を待機
	answerTimeout := time.After(5 * time.Second)
	answerChan := make(chan string)

	go func() {
		var answer map[string]string
		if err := conn.ReadJSON(&answer); err == nil {
			log.Printf("回答を受信: %+v", answer)
			answerChan <- answer["answer"]
		} else {
			log.Printf("回答受信エラー: %v", err)
		}
	}()

	select {
	case answer := <-answerChan:
		isCorrect := answer == correctAnswer
		log.Printf("回答結果: %v (正解: %s, 回答: %s)", isCorrect, correctAnswer, answer)

		resultMessage := map[string]interface{}{
			"status":         "answer_result",
			"correct":        isCorrect,
			"answer":         answer,
			"correct_answer": correctAnswer,
		}
		conn.WriteJSON(resultMessage)
		otherConn.WriteJSON(resultMessage)
		return isCorrect

	case <-answerTimeout:
		log.Printf("回答時間切れ")
		// タイムアウトメッセージを変更
		timeoutMessage := map[string]interface{}{
			"status":         "answer_result",
			"correct":        false,
			"answer":         "時間切れ",
			"correct_answer": correctAnswer,
		}
		conn.WriteJSON(timeoutMessage)
		otherConn.WriteJSON(timeoutMessage)
		return false
	}
}

func waitForMatch(room *Room) bool {
	// タイムアウト時間を30秒に延長
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		roomsMutex.Lock()
		if room.IsMatched {
			roomsMutex.Unlock()
			return true
		}
		roomsMutex.Unlock()

		select {
		case <-ticker.C:
			roomsMutex.Lock()
			if !room.IsMatched {
				delete(rooms, room.ID)
				room.Player1Conn.WriteJSON(map[string]string{
					"status": "timeout",
				})
				roomsMutex.Unlock()
				return false
			}
			roomsMutex.Unlock()
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// 勝者を決定する関数
func determineWinner(player1ID, player2ID string, score1, score2 int) map[string]string {
	if score1 > score2 {
		return map[string]string{
			"id":       player1ID,
			"loser_id": player2ID,
			"message":  "Player 1の勝利！",
		}
	} else if score2 > score1 {
		return map[string]string{
			"id":       player2ID,
			"loser_id": player1ID,
			"message":  "Player 2の勝利！",
		}
	}
	return map[string]string{
		"id":      "draw",
		"message": "引き分け",
	}
}

// レート計算と更新
func updatePlayerRatings(db *sql.DB, winnerID, loserID string) {
	if winnerID == "draw" {
		return // 引き分けの場合はレーティング更新なし
	}

	// レート更新のリクエストを作成
	rateRequest := rate.RatingRequest{
		WinnerID: winnerID,
		LoserID:  loserID,
		GameType: "quiz",
	}

	// レート計算ハンドラーを使用してレートを更新
	handler := rate.CalculateRatingHandler(db)

	// リクエストを作成
	reqBody, _ := json.Marshal(rateRequest)
	req, _ := http.NewRequest("POST", "/calculate-rating", bytes.NewBuffer(reqBody))

	// レスポンスを受け取るためのRecorderを作成
	w := httptest.NewRecorder()

	// ハンドラーを実行
	handler.ServeHTTP(w, req)

	// エラーチェック
	if w.Code != http.StatusOK {
		log.Printf("レート更新エラー: %v", w.Body.String())
		return
	}
}

// InitDB データベース接続を初期化する
func InitDB(database *sql.DB) {
	db = database
}
