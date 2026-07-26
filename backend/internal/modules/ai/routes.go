package ai

import (
	"iot-dashboard/internal/middleware"

	"github.com/gofiber/fiber/v2"
)

func RegisterRoutes(router fiber.Router) {
	ctrl := NewController()
	router.Use(middleware.Authenticate)

	router.Get("/tools", ctrl.ListTools)
	router.Post("/tools/execute", ctrl.ExecuteTool)

	router.Get("/conversations", ctrl.GetConversations)
	router.Post("/conversations", ctrl.CreateConversation)
	router.Get("/conversations/:id/messages", ctrl.GetMessages)
	router.Post("/conversations/:id/messages", ctrl.AddMessage)

	router.Get("/preview-draft", ctrl.GetPreviewDraft)
	router.Put("/preview-draft", ctrl.PutPreviewDraft)
	router.Delete("/preview-draft", ctrl.DeletePreviewDraft)
	router.Put("/selected-dashboard", ctrl.PutSelectedDashboard)

	router.Post("/chat", ctrl.Chat)

	// Ask-Data: NL → hardened SQL → ECharts, plus saved boards (nl2sql.go, boards.go)
	router.Get("/scopes", ListScopes)
	router.Post("/ask", AskData)
	router.Post("/run-sql", RunSQL)
	router.Get("/boards", ListBoards)
	router.Post("/boards", CreateBoard)
	router.Get("/boards/:id", GetBoard)
	router.Patch("/boards/:id", RenameBoard)
	router.Delete("/boards/:id", DeleteBoard)
	router.Post("/boards/:id/charts", AddBoardChart)
	router.Delete("/boards/:id/charts/:chartId", DeleteBoardChart)
}
