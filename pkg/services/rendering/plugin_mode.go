package rendering

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/grafana/grafana/pkg/plugins/backendplugin/pluginextensionv2"
)

func (rs *RenderingService) startPlugin(ctx context.Context) error {
	return rs.pluginInfo.Start(ctx)
}

func (rs *RenderingService) renderViaPlugin(ctx context.Context, renderKey string, opts Opts) (*RenderResult, error) {
	// gives plugin some additional time to timeout and return possible errors.
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout+time.Second*2)
	defer cancel()

	pngPath, err := rs.getFilePathForNewImage()
	if err != nil {
		return nil, err
	}

	headers := map[string]*pluginextensionv2.StringList{}

	for k, values := range opts.Headers {
		headers[k] = &pluginextensionv2.StringList{
			Values: values,
		}
	}

	req := &pluginextensionv2.RenderRequest{
		Url:               rs.getURL(opts.Path),
		Width:             int32(opts.Width),
		Height:            int32(opts.Height),
		DeviceScaleFactor: float32(opts.DeviceScaleFactor),
		FilePath:          pngPath,
		Timeout:           int32(opts.Timeout.Seconds()),
		RenderKey:         renderKey,
		Timezone:          isoTimeOffsetToPosixTz(opts.Timezone),
		Domain:            rs.domain,
		Headers:           headers,
	}
	rs.log.Debug("Calling renderer plugin", "req", req)

	rsp, err := rs.pluginInfo.GrpcPluginV2.Render(ctx, req)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rs.log.Info("Rendering timed out")
		return nil, ErrTimeout
	}
	if err != nil {
		return nil, err
	}
	if rsp.Error != "" {
		return nil, fmt.Errorf("rendering failed: %s", rsp.Error)
	}

	return &RenderResult{FilePath: pngPath}, err
}
