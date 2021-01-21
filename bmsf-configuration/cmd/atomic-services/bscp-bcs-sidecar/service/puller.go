/*
Tencent is pleased to support the open source community by making Blueking Container Service available.
Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
Licensed under the MIT License (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://opensource.org/licenses/MIT
Unless required by applicable law or agreed to in writing, software distributed under
the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
either express or implied. See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/viper"
	"golang.org/x/net/context"

	pbcommon "bk-bscp/internal/protocol/common"
	pb "bk-bscp/internal/protocol/connserver"
	"bk-bscp/internal/strategy"
	"bk-bscp/pkg/common"
	"bk-bscp/pkg/logger"
)

// Puller is config puller.
type Puller struct {
	viper *viper.Viper

	bizID string
	appID string
	path  string

	connSvrCli pb.ConnectionClient

	// config id.
	cfgID string

	// config release effect cache.
	effectCache *EffectCache

	// config content cache.
	contentCache *ContentCache

	// stop pulling sig channel.
	stopCh chan bool

	// publish event channel.
	ch chan interface{}
}

// NewPuller creates new Puller.
func NewPuller(viper *viper.Viper, bizID, appID, path, cfgID string,
	connSvrCli pb.ConnectionClient, effectCache *EffectCache, contentCache *ContentCache) *Puller {
	return &Puller{
		viper:        viper,
		bizID:        bizID,
		appID:        appID,
		path:         path,
		cfgID:        cfgID,
		connSvrCli:   connSvrCli,
		effectCache:  effectCache,
		contentCache: contentCache,
		stopCh:       make(chan bool, 1),
		ch:           make(chan interface{}, viper.GetInt("sidecar.configHandlerChSize")),
	}
}

// sidecarLabels marshals sidecar labels to string base on strategy protocol.
func (p *Puller) sidecarLabels() (string, error) {
	sidecarLabels := &strategy.SidecarLabels{
		Labels: p.viper.GetStringMapString(fmt.Sprintf("appmod.%s.labels", ModKey(p.bizID, p.appID, p.path))),
	}
	labels, err := json.Marshal(sidecarLabels)
	if err != nil {
		return "", err
	}
	return string(labels), nil
}

func (p *Puller) configFpath(fpath string) string {
	return filepath.Clean(
		fmt.Sprintf("%s/%s", p.viper.GetString(fmt.Sprintf("appmod.%s.path", ModKey(p.bizID, p.appID, p.path))), fpath))
}

// effect effects the release in notification.
func (p *Puller) effect(metadata *ReleaseMetadata) error {
	option := &PermissionOption{
		User:          metadata.User,
		UserGroup:     metadata.UserGroup,
		FilePrivilege: metadata.FilePrivilege,
		FileFormat:    metadata.FileFormat,
		FileMode:      metadata.FileMode,
	}

	// effect app real config.
	if err := p.contentCache.Effect(metadata.ContentID, metadata.CfgName,
		p.configFpath(metadata.CfgFpath), option); err != nil {

		if err := p.report(p.cfgID, metadata.ReleaseID, metadata.EffectTime, err); err != nil {
			logger.Warn("Puller[%s %s %s][%+v]| report configs local effect, %+v",
				p.bizID, p.appID, p.path, p.cfgID, err)
		}
		return err
	}

	// add effect cache.
	if err := p.effectCache.Effect(metadata); err != nil {
		if err := p.report(p.cfgID, metadata.ReleaseID, metadata.EffectTime, err); err != nil {
			logger.Warn("Puller[%s %s %s][%+v]| report configs local effect, %+v",
				p.bizID, p.appID, p.path, p.cfgID, err)
		}
		return err
	}

	// report local effected release information right now.
	if err := p.report(p.cfgID, metadata.ReleaseID, metadata.EffectTime, nil); err != nil {
		logger.Warn("Puller[%s %s %s][%+v]| effect the release success, but report configs local effected failed, %+v",
			p.bizID, p.appID, p.path, p.cfgID, err)
	}
	logger.Warnf("Puller[%s %s %s][%+v]| effect the release success, %+v", p.bizID, p.appID, p.path, p.cfgID, metadata)

	return nil
}

// pullTarget returns target release information.
func (p *Puller) pullTarget(targetRelease string) (bool, *pbcommon.Release, string, uint64, error) {
	return p.pullRelease(targetRelease)
}

// pullNewest returns the newest release information.
func (p *Puller) pullNewest() (bool, *pbcommon.Release, string, uint64, error) {
	return p.pullRelease("")
}

// pullRelease pulls release information from connserver.
func (p *Puller) pullRelease(target string) (bool, *pbcommon.Release, string, uint64, error) {
	// eliminate summit.
	common.DelayRandomMS(1500)

	// marshal sidecar labels.
	labels, err := p.sidecarLabels()
	if err != nil {
		return false, nil, "", 0, err
	}

	// local releaseID.
	md, err := p.effectCache.LocalRelease(p.cfgID)
	if err != nil {
		return false, nil, "", 0, err
	}

	if md == nil {
		md = &ReleaseMetadata{}
	}
	modKey := ModKey(p.bizID, p.appID, p.path)

	r := &pb.PullReleaseReq{
		Seq:            common.Sequence(),
		BizId:          p.viper.GetString(fmt.Sprintf("appmod.%s.bizid", modKey)),
		AppId:          p.viper.GetString(fmt.Sprintf("appmod.%s.appid", modKey)),
		CloudId:        p.viper.GetString(fmt.Sprintf("appmod.%s.cloudid", modKey)),
		Ip:             p.viper.GetString("appinfo.ip"),
		Path:           p.viper.GetString(fmt.Sprintf("appmod.%s.path", modKey)),
		Labels:         labels,
		CfgId:          p.cfgID,
		LocalReleaseId: md.ReleaseID,
		ReleaseId:      target,
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.viper.GetDuration("connserver.callTimeout"))
	defer cancel()

	logger.V(2).Infof("Puller[%s %s %s][%+v]| request to connserver PullRelease, %+v",
		p.bizID, p.appID, p.path, p.cfgID, r)

	resp, err := p.connSvrCli.PullRelease(ctx, r)
	if err != nil {
		return false, nil, "", 0, err
	}
	if resp.Code != pbcommon.ErrCode_E_OK {
		return false, nil, "", 0, errors.New(resp.Message)
	}
	return resp.NeedEffect, resp.Release, resp.ContentId, uint64(resp.ContentSize), nil
}

// handle target release in notification.
func (p *Puller) handlePubNotification(notification *pb.SCCMDPushNotification) (*ReleaseMetadata, error) {
	// check release publishing metadata.
	if notification == nil || notification.CfgId != p.cfgID {
		return nil, fmt.Errorf("invalid notification, %+v", notification)
	}

	// pull target release metadata in notification.
	needEffect, release, contentID, contentSize, err := p.pullTarget(notification.ReleaseId)
	if err != nil {
		return nil, fmt.Errorf("can't pull target release[%s], %+v", notification.ReleaseId, err)
	}
	if !needEffect {
		return nil, fmt.Errorf("pull target release[%s], no need to effect", notification.ReleaseId)
	}

	// check pull back release.
	if notification.CfgId != release.CfgId || notification.Serialno != release.Id {
		return nil, fmt.Errorf("pull target release, invalid release information, %+v, %+v", notification, release)
	}

	// release metadata pull back base on notification.
	metadata := &ReleaseMetadata{
		CfgID:          release.CfgId,
		CfgName:        release.CfgName,
		CfgFpath:       release.CfgFpath,
		User:           release.User,
		UserGroup:      release.UserGroup,
		FilePrivilege:  release.FilePrivilege,
		FileFormat:     release.FileFormat,
		FileMode:       release.FileMode,
		Serialno:       release.Id,
		ReleaseID:      notification.ReleaseId,
		ReleaseName:    release.Name,
		MultiReleaseID: release.MultiReleaseId,
		ContentID:      contentID,
		ContentSize:    contentSize,
		EffectTime:     time.Now().Format("2006-01-02 15:04:05"),
	}
	return metadata, nil
}

// pull newest to get right release now, even have not effected in local.
// The intention is just make the release version correct.
func (p *Puller) handleNewestRelease() (*ReleaseMetadata, error) {
	needEffect, release, contentID, contentSize, err := p.pullNewest()
	if err != nil {
		return nil, fmt.Errorf("can't pull newest release, %+v", err)
	}
	if !needEffect {
		return nil, errors.New("pull newest release, no newest need to effect")
	}

	// release metadata.
	metadata := &ReleaseMetadata{
		CfgID:          release.CfgId,
		CfgName:        release.CfgName,
		CfgFpath:       release.CfgFpath,
		User:           release.User,
		UserGroup:      release.UserGroup,
		FilePrivilege:  release.FilePrivilege,
		FileFormat:     release.FileFormat,
		FileMode:       release.FileMode,
		Serialno:       release.Id,
		ReleaseID:      release.ReleaseId,
		ReleaseName:    release.Name,
		MultiReleaseID: release.MultiReleaseId,
		ContentID:      contentID,
		ContentSize:    contentSize,
		EffectTime:     time.Now().Format("2006-01-02 15:04:05"),
	}
	return metadata, nil
}

// pulling keeps pulling configs.
func (p *Puller) pulling() {
	modKey := ModKey(p.bizID, p.appID, p.path)

	// need to keeping fast auto pull for no local released config or that have
	// local release but need to sync again to get the newest release version.
	isFastAutoPullDone := false

	for {
		if p.viper.GetBool(fmt.Sprintf("appmod.%s.stop", modKey)) {
			logger.Warnf("Puller[%s %s %s][%+v]| stop pulling now!", p.bizID, p.appID, p.path, p.cfgID)
			return
		}

		var metadata *ReleaseMetadata

		// effect with serial num, unless rollback or newest logic.
		needEffectWithSerialNo := true
		autoPullInterval := p.viper.GetDuration("sidecar.pullConfigInterval")
		maxEmptyConfigPullInterval := int(p.viper.GetDuration("sidecar.maxEmptyConfigPullInterval") / time.Millisecond)

		currentRelease, err := p.effectCache.LocalRelease(p.cfgID)
		if err != nil || currentRelease == nil {
			// keeping fast auto pull for no local released config.
			autoPullInterval = common.RandomMS(maxEmptyConfigPullInterval)
			logger.V(4).Infof("Puller[%s %s %s][%+v]-pulling| no local effeced release, auto pull now[%+v]!",
				p.bizID, p.appID, p.path, p.cfgID, autoPullInterval)

		} else {
			// fast auto pull for config which have local release but need to
			// sync again to get the newest release version.
			if !isFastAutoPullDone {
				autoPullInterval = common.RandomMS(5000)
				isFastAutoPullDone = true
			}
		}

		select {
		// stop pulling signal.
		case <-p.stopCh:
			logger.Warn("Puller[%s %s %s][%+v]-pulling| stop pulling now", p.bizID, p.appID, p.path, p.cfgID)
			return

		// handle publishing notifications.
		case notification := <-p.ch:

			// handle multi type notifications.
			switch notification.(type) {
			case *pb.SCCMDPushNotification:

				// normal release publish notification.
				pubNotification := notification.(*pb.SCCMDPushNotification)

				md, err := p.handlePubNotification(pubNotification)
				if err != nil {
					logger.Warn("Puller[%s %s %s][%+v]-pulling| handle publish, %+v",
						p.bizID, p.appID, p.path, p.cfgID, err)
					continue
				}
				metadata = md

				logger.Warn("Puller[%s %s %s][%+v]-pulling| recviced publishing notification, metadata %+v",
					p.bizID, p.appID, p.path, p.cfgID, metadata)

			case *pb.SCCMDPushRollbackNotification:

				// release rollback publishing notification.
				rollbackNotification := notification.(*pb.SCCMDPushRollbackNotification)

				logger.Warnf("Puller[%s %s %s][%+v]-pulling| recviced rollback publishing notification, %+v",
					p.bizID, p.appID, p.path, p.cfgID, rollbackNotification)

				// need effect without serial num(rollback event).
				needEffectWithSerialNo = false

				md, err := p.handleNewestRelease()
				if err != nil {
					logger.Warn("Puller[%s %s %s][%+v]-pulling| handle rollback publish, %+v",
						p.bizID, p.appID, p.path, p.cfgID, err)
					continue
				}
				metadata = md
				metadata.isRollback = true

				logger.Warn("Puller[%s %s %s][%+v]-pulling| rollback publish, newest release, %+v",
					p.bizID, p.appID, p.path, p.cfgID, metadata)

			default:
				logger.Error("Puller[%s %s %s][%+v]-pulling| unknow notification[%+v]",
					p.bizID, p.appID, p.path, p.cfgID, notification)
			}

		case <-time.After(autoPullInterval):

			// newest logic need effect without serial num(rollback event).
			needEffectWithSerialNo = false

			md, err := p.handleNewestRelease()
			if err != nil {
				logger.Warn("Puller[%s %s %s][%+v]-pulling| handle pull newest, %+v",
					p.bizID, p.appID, p.path, p.cfgID, err)
				continue
			}
			metadata = md

			logger.Warn("Puller[%s %s %s][%+v]-pulling| recviced newest release, %+v",
				p.bizID, p.appID, p.path, p.cfgID, metadata)
		}

		// check if need to effect this release.
		// Newest release and rollback logic no need to effect with serial num.
		if needEffectWithSerialNo {
			// compare local release serial num.
			needEffect, err := p.effectCache.NeedEffected(metadata.CfgID, metadata.Serialno)
			if err != nil {
				logger.Error("Puller[%s %s %s][%+v]-pulling| check local effect information, %+v",
					p.bizID, p.appID, p.path, p.cfgID, err)
				continue
			}
			if !needEffect {
				logger.Warn("Puller[%s %s %s][%+v]-pulling| finally, no need to effect the release, %+v",
					p.bizID, p.appID, p.path, p.cfgID, metadata)
				continue
			}
		}

		// mark event type.
		lmd, err := p.effectCache.LocalRelease(metadata.CfgID)
		if err != nil {
			logger.Warn("Puller[%s %s %s][%+v]-pulling| mark event type, %+v", p.bizID, p.appID, p.path, p.cfgID, err)
		} else if lmd != nil {
			if metadata.isRollback || metadata.Serialno < lmd.Serialno {
				// recved a rollback publishing or pull newest on time get an old release.
				metadata.isRollback = true
			}
		}

		// check local file content cache.
		if cached, err := p.contentCache.Has(metadata.ContentID); err == nil && cached {
			logger.Warn("Puller[%s %s %s][%+v]-pulling| has the content cache[%+v], and effect right now.",
				p.bizID, p.appID, p.path, p.cfgID, metadata.ContentID)

			if err := p.effect(metadata); err != nil {
				logger.Error("Puller[%s %s %s][%+v]-pulling| after cache checking, can't effect release, %+v",
					p.bizID, p.appID, p.path, p.cfgID, err)
			}
			continue
		}

		// add config content cache.
		logger.Warn("Puller[%s %s %s][%+v]-pulling| pull release[%+v] back, add content cache now.",
			p.bizID, p.appID, p.path, p.cfgID, metadata.ReleaseID)

		if err := p.contentCache.Add(&Content{ContentID: metadata.ContentID}); err != nil {
			logger.Error("Puller[%s %s %s][%+v]-pulling| add config content cache, %+v.",
				p.bizID, p.appID, p.path, p.cfgID, err)
			continue
		}

		// effect this release now.
		if err := p.effect(metadata); err != nil {
			logger.Error("Puller[%s %s %s][%+v]-pulling| after adding cache, can't effect release, %+v",
				p.bizID, p.appID, p.path, p.cfgID, err)
		}

		// loop end.
		continue
	}
}

// report reports release effect information.
func (p *Puller) report(cfgID, releaseID, effectTime string, effectErr error) error {
	reportInfos := []*pbcommon.ReportInfo{}

	reportInfo := &pbcommon.ReportInfo{
		CfgId:      cfgID,
		ReleaseId:  releaseID,
		EffectTime: effectTime,
	}

	if effectErr == nil {
		reportInfo.EffectCode = EffectCodeSuccess
		reportInfo.EffectMsg = EffectMsgSuccess
	} else {
		reportInfo.EffectCode = EffectCodeFailed
		reportInfo.EffectMsg = effectErr.Error()
	}
	reportInfos = append(reportInfos, reportInfo)

	// marshal sidecar labels.
	labels, err := p.sidecarLabels()
	if err != nil {
		return err
	}
	modKey := ModKey(p.bizID, p.appID, p.path)

	r := &pb.ReportReq{
		Seq:     common.Sequence(),
		BizId:   p.viper.GetString(fmt.Sprintf("appmod.%s.bizid", modKey)),
		AppId:   p.viper.GetString(fmt.Sprintf("appmod.%s.appid", modKey)),
		CloudId: p.viper.GetString(fmt.Sprintf("appmod.%s.cloudid", modKey)),
		Ip:      p.viper.GetString("appinfo.ip"),
		Path:    p.viper.GetString(fmt.Sprintf("appmod.%s.path", modKey)),
		Labels:  labels,
		Infos:   reportInfos,
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.viper.GetDuration("connserver.callTimeout"))
	defer cancel()

	logger.V(2).Infof("Puller[%s %s %s][%+v]| request to connserver Report, %+v", p.bizID, p.appID, p.path, p.cfgID, r)

	resp, err := p.connSvrCli.Report(ctx, r)
	if err != nil {
		return err
	}
	if resp.Code != pbcommon.ErrCode_E_OK {
		return errors.New(resp.Message)
	}
	return nil
}

// HandlePub handles release publishing notification.
func (p *Puller) HandlePub(notification *pb.SCCMDPushNotification) error {
	if notification == nil {
		return errors.New("invalid publish notification struct: nil")
	}

	select {
	case p.ch <- notification:
	case <-time.After(p.viper.GetDuration("sidecar.configHandlerChTimeout")):
		return fmt.Errorf("send cmd to config handler puller channel timeout, %+v", notification)
	}

	return nil
}

// HandleRoll handles release rollback publishing notification.
func (p *Puller) HandleRoll(notification *pb.SCCMDPushRollbackNotification) error {
	if notification == nil {
		return errors.New("invalid rollback notification struct: nil")
	}

	select {
	case p.ch <- notification:
	case <-time.After(p.viper.GetDuration("sidecar.configHandlerChTimeout")):
		return fmt.Errorf("send cmd to config handler puller channel timeout, %+v", notification)
	}

	return nil
}

func (p *Puller) deleteConfig() error {
	if !p.viper.GetBool("sidecar.enableDeleteConfig") {
		return nil
	}

	md, err := p.effectCache.LocalRelease(p.cfgID)
	if err != nil {
		return err
	}
	if md == nil {
		return errors.New("no local effect release metadata")
	}

	if len(md.CfgFpath) == 0 {
		return errors.New("local effect release metadata: invalid config fpath")
	}

	if len(md.CfgName) == 0 {
		return errors.New("local effect release metadata: invalid config name")
	}

	// delete config.
	targetFile := fmt.Sprintf("%s/%s", p.configFpath(md.CfgFpath), md.CfgName)
	trashFile := fmt.Sprintf("%s/%s", p.viper.GetString("cache.contentExpiredPath"), md.CfgName)

	if err := os.Rename(targetFile, trashFile); err != nil {
		return err
	}
	return nil
}

// Stop stops the puller.
func (p *Puller) Stop() {
	select {
	case p.stopCh <- true:
		if err := p.deleteConfig(); err != nil {
			logger.Errorf("Puller[%s %s %s][%+v]| delete config failed when stop the puller.", p.bizID, p.appID, p.path, p.cfgID)
		}

	case <-time.After(time.Second):
		logger.Warn("Puller[%s %s %s][%+v]| stop puller timeout.", p.bizID, p.appID, p.path, p.cfgID)
	}
}

// Run runs the puller.
func (p *Puller) Run() {
	go p.pulling()
}