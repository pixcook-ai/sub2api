package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

func (h *OpenAIGatewayHandler) SetManagedFileResolver(resolver *service.ManagedFileResolver) {
	h.managedFiles = resolver
	h.gatewayService.SetManagedFileResolver(resolver)
}
