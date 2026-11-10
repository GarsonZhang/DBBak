(() => {
  'use strict';
  const $ = (s, p=document) => p.querySelector(s);
  const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const ico = (n,s) => window.icon(n,s);
  const main = $('#main'), modal = $('#modal');
  const pages = {overview:'备份概览',tasks:'备份任务',databases:'数据库',storage:'存储位置',logs:'执行日志',cleanup:'保留与清理',deployment:'部署与执行器',settings:'系统设置'};
  const state = {user:null,page:location.hash.slice(1) in pages?location.hash.slice(1):'overview',sources:[],targets:[],jobs:[],runs:[],artifacts:[],overview:null,capabilities:null,settings:null,search:'',logFilter:'all',cleanupJobID:'',cleanupPreview:null,loading:false,error:'',removal:null};

  class APIError extends Error { constructor(status,code,message){super(message);this.status=status;this.code=code;} }
  async function api(path,options={}) {
    const headers={'Accept':'application/json','X-DBBak-Request':'1',...(options.headers||{})};
    // 浏览器只允许请求头携带 ISO-8859-1，中文任务名/路径必须走查询参数或请求体。
    // 在这里提前拦下，给出可读提示，而不是让 fetch 抛 “non ISO-8859-1 code point”。
    for(const [key,value] of Object.entries(headers)){if(!/^[\x20-\x7e]*$/.test(String(value)))throw new APIError(0,'INVALID_HEADER',`请求头 ${key} 含非 ASCII 字符，请改用查询参数或请求体传递。`);}
    if(options.body&&!headers['Content-Type'])headers['Content-Type']='application/json; charset=utf-8';
    const response=await fetch(path,{...options,headers,credentials:'same-origin'});
    const text=await response.text();let payload=null;
    if(text){try{payload=JSON.parse(text);}catch{throw new APIError(response.status,'INVALID_RESPONSE','服务端返回了无效 JSON');}}
    if(!response.ok){if(response.status===401&&path!=='/api/v1/auth/login')showLogin();throw new APIError(response.status,payload?.code||'REQUEST_FAILED',payload?.message||`请求失败 (${response.status})`);}
    return payload;
  }
  async function loadAll({quiet=false}={}) {
    if(!state.user)return;if(!quiet)state.loading=true;state.error='';
    try{
      const [sources,targets,jobs,runs,artifacts,overview,capabilities,settings]=await Promise.all([api('/api/v1/sources'),api('/api/v1/storage-targets'),api('/api/v1/jobs'),api('/api/v1/runs'),api('/api/v1/artifacts'),api('/api/v1/overview'),api('/api/v1/capabilities'),api('/api/v1/settings')]);
      Object.assign(state,{sources,targets,jobs,runs,artifacts,overview,capabilities,settings});
      if(!state.cleanupJobID&&jobs.length)state.cleanupJobID=jobs[0].id;
    }catch(error){state.error=error.message;}finally{state.loading=false;if(state.user)render();}
  }
  function showLogin(){
    state.user=null;document.body.classList.add('auth-mode');let screen=$('#login-screen');
    if(!screen){screen=document.createElement('main');screen.id='login-screen';screen.innerHTML=`<section class="login-panel"><div class="login-brand"><span class="brand-mark">${ico('shield',30)}</span><span>DBBak</span></div><p class="page-eyebrow">DBBAK / PHASE 2</p><h1>数据库备份管理</h1><p>登录真实管理服务。当前支持 MySQL 逻辑全量与 PostgreSQL 整实例物理全量。</p><form id="login-form"><label class="field">管理员用户名<input name="username" required maxlength="64" autocomplete="username" value="admin"></label><label class="field">管理员密码<input name="password" type="password" required autocomplete="current-password"></label><p class="form-error" id="login-error" role="alert"></p><button class="btn btn-primary" type="submit">登录管理服务</button></form><p class="hint">会话通过 HttpOnly Cookie 保存，密码不会写入浏览器存储。</p></section>`;document.body.appendChild(screen);}
    screen.style.display='grid';setTimeout(()=>$('#login-form input[name="password"]')?.focus(),0);
  }
  function showApp(user){state.user=user;document.body.classList.remove('auth-mode');$('#login-screen')?.remove();}
  function toast(message,error=false){const e=$('#toast');
    // popover 处于顶层，弹窗打开时也能完整显示；关闭时清空计时器避免反复开关抖动。
    if(!e.matches(':popover-open')){try{e.showPopover();}catch{}}
    e.textContent=message;e.className=error?'show error':'show';
    clearTimeout(toast.timer);
    toast.timer=setTimeout(()=>{e.className='';try{if(e.matches(':popover-open'))e.hidePopover();}catch{}},error?7000:4200);}
  // 弹窗内的表单错误必须贴在弹窗里：toast 在 <dialog> 之外，弹窗打开时用户
  // 要么看不到，要么得先把视线从遮罩下方找回来。有弹窗就在表单顶部插入一条，
  // 没有弹窗才退回 toast。返回 false 表示错误未显示，便于调用方决定是否再提示。
  function showFormError(error){
    const message=`${error?.code&&error.code!=='REQUEST_FAILED'?error.code+': ':''}${error?.message||'操作失败'}`;
    const body=modal?.open?modal.querySelector('.modal-body'):null;
    if(!body){toast(message,true);return;}
    let box=body.querySelector('.form-alert');
    if(!box){box=document.createElement('div');box.className='form-alert';box.setAttribute('role','alert');body.prepend(box);}
    box.textContent=message;
    box.scrollIntoView({block:'nearest'});
    // 弹窗内容可滚动时把出错位置带回可视区，避免用户以为点了没反应。
    body.scrollTo({top:0,behavior:'smooth'});
  }
  function clearFormError(){modal?.querySelector('.form-alert')?.remove();}
  // 任务时区候选。默认给国内常用时区，同时列出浏览器所在时区；
  // 后端用 IANA 名称解析，所以这里只提供标准名，避免用户手填出无效值。
  function timezoneOptions(current){
    const list=['Asia/Shanghai','Asia/Hong_Kong','Asia/Taipei','Asia/Tokyo','Asia/Seoul','Asia/Singapore','UTC','Europe/London','Europe/Berlin','America/New_York','America/Los_Angeles'];
    const local=Intl.DateTimeFormat().resolvedOptions().timeZone;
    const names=[...new Set([current||'Asia/Shanghai',...(local&&!list.includes(local)?[local]:[]),...list])];
    return names.map(name=>`<option value="${esc(name)}" ${name===(current||'Asia/Shanghai')?'selected':''}>${esc(name)}</option>`).join('');
  }
  const badge=(text,kind='')=>`<span class="badge ${kind}">${esc(text)}</span>`;
  const title=(name,sub,action='')=>`<div class="page-header"><div><div class="page-eyebrow">DBBAK / ${esc(pages[state.page])}</div><h1>${esc(name)}</h1><p class="subtitle">${esc(sub)}</p></div>${action}</div>`;
  const empty=(text,detail='调整筛选条件，或先创建一个配置。')=>`<div class="empty-state">${ico('search',30)}<h3>${esc(text)}</h3><p>${esc(detail)}</p></div>`;
  function fmt(value){if(!value)return'—';return new Intl.DateTimeFormat('zh-CN',{timeZone:'Asia/Shanghai',year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit'}).format(new Date(value));}
  function bytes(value){const n=Number(value||0);if(n<1024)return`${n} B`;if(n<1048576)return`${(n/1024).toFixed(1)} KiB`;if(n<1073741824)return`${(n/1048576).toFixed(1)} MiB`;return`${(n/1073741824).toFixed(2)} GiB`;}
  const source=id=>state.sources.find(x=>x.id===id),target=id=>state.targets.find(x=>x.id===id),job=id=>state.jobs.find(x=>x.id===id);
  const activeRun=id=>state.runs.find(r=>r.job_id===id&&['queued','running','verifying','reconciling'].includes(r.state));
  function runBadge(run){const labels={queued:'等待执行',running:'执行中',verifying:'文件校验中',succeeded:'成功',failed:'失败',interrupted:'已中断',missed:'错过计划',cancelled:'已取消',reconciling:'对账中'};const kinds={succeeded:'',failed:'warning',interrupted:'warning',missed:'muted',queued:'blue',running:'blue',verifying:'blue'};return badge(labels[run.state]||run.state,kinds[run.state]||'muted');}
  const errorBanner=()=>state.error?`<div class="notice error-banner">${ico('info')}<span>部分数据加载失败：${esc(state.error)} <button class="btn btn-small" data-action="refresh">重试</button></span></div>`:'';

  function renderOverview(){const o=state.overview||{job_count:0,recent_runs:[],verified_artifacts:0,verified_bytes:0};const enabled=state.jobs.filter(x=>x.enabled).length,failed=state.runs.filter(x=>x.state==='failed').length;return`${title('备份概览','真实配置、真实运行与真实文件，不再使用浏览器样例数据。',`<button class="btn btn-primary" data-action="new-task">${ico('plus')}新建备份任务</button>`)}<div class="notice">${ico('info')}<span>当前支持 MySQL 逻辑全量与 PostgreSQL 整实例物理全量；差异和日志链仍未实现。</span></div><div class="metric-grid"><section class="metric-card"><span>备份任务</span><strong>${o.job_count}</strong><small>${enabled} 个已启用</small></section><section class="metric-card"><span>已校验备份</span><strong>${o.verified_artifacts}</strong><small>${bytes(o.verified_bytes)}</small></section><section class="metric-card"><span>最近失败</span><strong>${failed}</strong><small>最近 100 条运行</small></section><section class="metric-card"><span>数据源</span><strong>${state.sources.length}</strong><small>凭据由服务端加密保存</small></section></div><section class="panel"><div class="panel-header"><h2 class="panel-title">最近执行</h2><button class="btn btn-small" data-nav="logs">全部日志</button></div>${runTable(o.recent_runs||[])}</section>`;}
  function filteredJobs(){const term=state.search.toLowerCase();return state.jobs.filter(x=>!term||`${x.name} ${source(x.source_id)?.name||''}`.toLowerCase().includes(term));}
  function jobTable(jobs){if(!jobs.length)return empty('没有符合条件的备份任务','先添加数据源，再创建任务。');return`<div class="table-wrap"><table class="data-table"><thead><tr><th>任务 / 数据源</th><th>计划</th><th>保留</th><th>下次执行</th><th>状态</th><th></th></tr></thead><tbody>${jobs.map(item=>{const running=activeRun(item.id),schedule=item.schedule_type==='interval'?`每 ${item.interval_minutes} 分钟`:`${item.schedule_type==='weekly'?'每周':'每日'} ${item.time_of_day}`;return`<tr><td><span class="cell-title">${esc(item.name)}</span><span class="cell-sub">${item.backup_type==='differential'?'差异':item.backup_type==='log'?'事务日志':'全量'}${item.backup_method==='physical'?' · 物理 XtraBackup':''} · ${esc(source(item.source_id)?.name||'数据源不存在')} → ${item.location_kind==='database_server'?('数据库服务器 '+esc(item.server_dir)):esc(target(item.target_id)?.root||'存储目录已删除')}</span><span class="cell-sub mono">${esc(item.subdir?'子目录 '+item.subdir:(item.location_kind==='database_server'?'服务器目录':'存储根目录'))}/${esc(source(item.source_id)?.database||'')}</span></td><td>${esc(schedule)}<span class="cell-sub">${esc(item.timezone)}</span></td><td>${item.retention_days} 天 / ${item.min_full} 份</td><td>${item.enabled?fmt(item.next_run_at):'已暂停'}</td><td>${running?runBadge(running):badge(item.enabled?'已启用':'已暂停',item.enabled?'':'muted')}</td><td><div class="row-actions"><button class="btn btn-small" data-action="run" data-id="${item.id}" ${running?'disabled':''}>${ico('play',14)}立即执行</button><button class="btn btn-ghost btn-small" data-action="edit-task" data-id="${item.id}">编辑</button><button class="btn btn-ghost btn-small danger-action" data-action="delete-job" data-id="${item.id}" title="删除任务">${ico('trash',14)}删除</button></div></td></tr>`;}).join('')}</tbody></table></div>`;}
  function renderTasks(){return`${title('备份任务','按数据库实际能力配置全量计划；未实现的差异与日志不会出现在表单中。',`<button class="btn btn-primary" data-action="new-task">${ico('plus')}新建备份任务</button>`)}<section class="panel"><div class="toolbar"><label class="search-field">${ico('search',16)}<input id="task-search" placeholder="搜索任务或数据源…" value="${esc(state.search)}" aria-label="搜索备份任务"></label><span class="subtle toolbar-end">共 ${filteredJobs().length} 个任务</span></div><div id="job-table">${jobTable(filteredJobs())}</div></section>`;}
  function renderDatabases(){return`${title('数据库','MySQL 与 PostgreSQL 连接测试由服务端调用真实客户端完成。',`<button class="btn btn-primary" data-action="add-db">${ico('plus')}添加数据源</button>`)}<div class="notice">${ico('shield')}<span>密码只用于服务端加密保存和真实连接，不写入 localStorage，不会随列表接口返回。</span></div>${state.sources.length?`<div class="grid-3">${state.sources.map(item=>`<section class="panel database-card"><div class="panel-header"><div><h2 class="panel-title">${esc(item.name)}</h2><p class="panel-subtitle">${item.engine==='postgresql'?'PostgreSQL · 整实例物理全量':item.engine==='sqlserver'?'SQL Server · 原生全量':'MySQL · 逻辑 / 物理全量'}</p></div>${badge('真实数据源','muted')}</div><div class="detail-list"><div><span>服务器</span><code>${esc(item.host)}:${item.port}</code></div><div><span>${item.engine==='postgresql'?'维护库（整实例备份）':'数据库'}</span><strong>${esc(item.database)}</strong></div><div><span>账号</span><strong>${esc(item.username)}</strong></div><div><span>关联任务</span><strong>${state.jobs.filter(j=>j.source_id===item.id).length} 个</strong></div></div><div class="card-footer"><button class="btn btn-small" data-action="probe" data-id="${item.id}">${ico('check',14)}连接测试</button><button class="btn btn-ghost btn-small" data-action="edit-db" data-id="${item.id}">编辑</button><button class="btn btn-ghost btn-small danger-action" data-action="delete-db" data-id="${item.id}" title="删除数据源">${ico('trash',14)}删除</button></div></section>`).join('')}</div>`:empty('尚未配置数据源')}`;}
  function storageKindLabel(kind){return kind==='ftp'?'FTP 远程':kind==='oss'?'对象存储':'本地执行器';}
  function renderStorage(){return`${title('存储位置','本地目录在任务中直接填写并自动登记；FTP 与对象存储（阿里云 OSS / S3 兼容）在本页登记，保存时执行真实写检查。',`<button class="btn btn-primary" data-action="add-location">${ico('plus')}登记存储位置</button>`)}${state.targets.length?`<div class="grid-3">${state.targets.map(item=>`<section class="panel storage-card"><div class="panel-header"><span class="storage-icon">${ico('harddrive',25)}</span>${badge(storageKindLabel(item.kind),'muted')}</div><div class="storage-content"><h2>${esc(item.name)}</h2><p>${item.kind==='ftp'?`FTP · ${esc(item.host)}:${item.port||21}`:item.kind==='oss'?`对象存储 · ${esc(item.bucket)} @ ${esc(item.endpoint)}`:item.kind==='unc'?'共享目录（SMB）· 已授权根路径':'本地目录 · 已授权根路径'}</p><code class="path-value">${item.kind==='oss'?`${esc(item.root||'（桶根）')}`:esc(item.root)}</code>${item.kind==='oss'&&item.use_ssl===false?`<p class="cell-sub">未启用 HTTPS</p>`:''}${item.server_root?`<code class="path-value">服务端 ${esc(item.server_root)}</code>`:''}<p class="cell-sub">${state.artifacts.filter(f=>f.target_id===item.id&&f.state==='verified').length} 份已校验备份</p></div><div class="card-footer"><button class="btn btn-small" data-action="check-storage" data-id="${item.id}">${ico('check',14)}写权限检查</button><button class="btn btn-ghost btn-small" data-action="edit-location" data-id="${item.id}">编辑</button><button class="btn btn-ghost btn-small danger-action" data-action="delete-location" data-id="${item.id}" title="删除存储位置">${ico('trash',14)}删除</button></div></section>`).join('')}</div>`:empty('尚无登记的存储目录','在备份任务中填写本地目录后会自动登记。')}<div class="notice storage-notice">${ico('shield')}<span>清理只允许删除已登记的普通文件；已有备份的目录不能修改根路径。不同实例的落点由各备份任务的「输出子目录」决定，同一目录可被多个任务共用。对象存储没有目录语义，「对象键前缀」只做键前缀拼接；账号权限建议收敛到该前缀与备份桶。</span></div>`;}
  function runTable(runs){if(!runs.length)return empty('暂无执行记录','创建任务后，可手动执行或等待后台调度。');return`<div class="table-wrap"><table class="data-table"><thead><tr><th>任务 / 运行</th><th>触发</th><th>开始时间</th><th>命令 / 文件 / 恢复</th><th>状态</th><th></th></tr></thead><tbody>${runs.map(run=>`<tr><td><span class="cell-title">${esc(job(run.job_id)?.name||'任务已删除')}</span><span class="cell-sub mono">${esc(run.id)}</span></td><td>${run.trigger_type==='scheduled'?'计划':'手动'}</td><td>${fmt(run.started_at||run.created_at)}</td><td><span class="cell-sub">${esc(run.command_status)} / ${esc(run.file_status)} / ${esc(run.restore_status)}</span>${run.error_message?`<span class="cell-sub error-text">${esc(run.error_message)}</span>`:''}</td><td>${runBadge(run)}</td><td><button class="btn btn-ghost btn-small" data-action="view-log" data-id="${run.id}">详情 ${ico('arrow',14)}</button></td></tr>`).join('')}</tbody></table></div>`;}
  function renderLogs(){const runs=state.runs.filter(r=>state.logFilter==='all'||r.state===state.logFilter);return`${title('执行日志','运行、错误、文件校验和恢复验证均来自 SQLite。')}<section class="panel"><div class="toolbar"><div class="segmented">${[['all','全部'],['succeeded','成功'],['failed','失败'],['running','执行中'],['interrupted','已中断']].map(([v,l])=>`<button class="${state.logFilter===v?'active':''}" data-action="log-filter" data-value="${v}">${l}</button>`).join('')}</div><span class="subtle toolbar-end">${runs.length} 条记录</span></div>${runTable(runs)}</section>`;}
  function renderCleanup(){const selected=job(state.cleanupJobID),preview=state.cleanupPreview;return`${title('保留与清理','先预览恢复点，再由服务端复核版本、依赖、真实路径和文件摘要。')}<div class="overview-grid cleanup-layout"><section class="panel"><div class="panel-header"><h2 class="panel-title">任务保留策略</h2>${badge('独立全量恢复点')}</div>${selected?`<form id="retention-form" class="settings-form"><label class="field">选择任务<select id="cleanup-job">${state.jobs.map(x=>`<option value="${x.id}" ${x.id===state.cleanupJobID?'selected':''}>${esc(x.name)}</option>`).join('')}</select></label><div class="form-grid"><label class="field">保留天数<input name="retention_days" type="number" min="1" max="3650" value="${selected.retention_days}" required></label><label class="field">最低全量份数<input name="min_full" type="number" min="1" max="100" value="${selected.min_full}" required></label></div><p class="hint">保存策略会使旧预览失效。自动清理仅处理独立全量，并由调度器在每天 05:00 后复查执行。</p><button class="btn" type="submit">保存保留策略</button><button class="btn btn-primary" type="button" data-action="preview-cleanup">${ico('search')}生成清理预览</button></form>`:empty('请先创建备份任务')}</section><section class="panel cleanup-summary"><span class="metric-icon">${ico('shield',25)}</span><h2>释放空间之前<br>先证明可以删除</h2><p>保留窗口、最低份数、活动运行、未校验文件和人工锁定均受保护。</p><div class="detail-list"><div><span>备份记录</span><strong>${state.artifacts.filter(x=>x.state!=='deleted').length} 个</strong></div><div><span>人工锁定</span><strong>${state.artifacts.filter(x=>x.locked&&x.state!=='deleted').length} 个</strong></div></div></section></div>${preview?cleanupPreviewHTML(preview):`<div class="empty-state cleanup-empty">${ico('folder',32)}<h3>先预览，再清理</h3><p>预览有效期 10 分钟；任何配置或文件状态变化都会使其失效。</p></div>`}${artifactTable(selected?.id)}`;}
  function cleanupPreviewHTML(preview){const r=preview.result,candidates=r.items.filter(x=>x.candidate);return`<section class="panel cleanup-results"><div class="panel-header"><div><h2 class="panel-title">清理预览</h2><p class="panel-subtitle">${candidates.length} 个候选 · 预计释放 ${bytes(r.candidate_size)} · ${fmt(preview.expires_at)} 前有效</p></div><button class="btn btn-danger" data-action="confirm-cleanup" ${candidates.length?'':'disabled'}>${ico('trash',16)}确认清理</button></div><div class="table-wrap"><table class="data-table"><thead><tr><th>文件</th><th>备份时间</th><th>大小</th><th>评估</th></tr></thead><tbody>${r.items.map(item=>`<tr><td><span class="cell-title">${esc(item.artifact.relative_path)}</span><span class="cell-sub mono">${esc(item.artifact.id)}</span></td><td>${fmt(item.artifact.created_at)}</td><td>${bytes(item.artifact.size)}</td><td>${badge(item.candidate?'可清理':'受保护',item.candidate?'warning':'muted')}<span class="cell-sub">${esc(item.reason)}</span></td></tr>`).join('')}</tbody></table></div></section>`;}
  function artifactTable(jobID){const artifacts=state.artifacts.filter(x=>(!jobID||x.job_id===jobID)&&x.state!=='deleted');if(!artifacts.length)return'';return`<section class="panel"><div class="panel-header"><h2 class="panel-title">备份文件记录</h2><p class="panel-subtitle">命令完成不等于文件可用，文件可用不等于已恢复验证</p></div><div class="table-wrap"><table class="data-table"><thead><tr><th>对象</th><th>方法 / 工具</th><th>大小 / SHA-256</th><th>状态</th><th></th></tr></thead><tbody>${artifacts.map(item=>`<tr><td><span class="cell-title">${esc(item.relative_path)}</span><span class="cell-sub">${fmt(item.created_at)}</span></td><td>${esc(item.method)}<span class="cell-sub">${esc(item.tool_version)}</span></td><td>${bytes(item.size)}<span class="cell-sub mono">${esc(item.sha256)}</span></td><td>${badge(item.state,item.state==='verified'?'':'warning')}${item.locked?badge('人工锁定','muted'):''}</td><td><button class="btn btn-ghost btn-small" data-action="hold" data-id="${item.id}" data-locked="${item.locked}">${item.locked?'解除锁定':'锁定保留'}</button><button class="btn btn-ghost btn-small danger-action" data-action="delete-artifact" data-id="${item.id}">${ico('trash',14)}删除</button></td></tr>`).join('')}</tbody></table></div></section>`;}
  function renderDeployment(){const c=state.capabilities;return`${title('部署与执行器','当前运行的是单实例控制服务与本地执行器。')}<div class="notice">${ico('info')}<span>Windows 服务、Linux systemd、Docker 发行包和远程 Agent 属于后续阶段，当前未验收。</span></div><section class="panel"><div class="panel-header"><h2 class="panel-title">本地执行器</h2>${badge(c?.local_executor?.status||'未知',c?.local_executor?.status==='supported'?'':'warning')}</div><div class="detail-list settings-form"><div><span>MySQL 工具</span><strong>${esc(Object.entries(c?.local_executor?.tool_versions||{}).map(([k,v])=>k+': '+v).join(' | ')||c?.local_executor?.message||'检测中')}</strong></div><div><span>执行方式</span><strong>参数数组调用 · 无 shell 拼接</strong></div><div><span>后台调度</span><strong>由 DBBak 服务进程运行，与浏览器无关</strong></div></div></section><section class="panel"><div class="panel-header"><h2 class="panel-title">能力矩阵</h2></div><div class="table-wrap"><table class="data-table"><thead><tr><th>引擎</th><th>方法</th><th>类型</th><th>状态</th></tr></thead><tbody>${(c?.capabilities||[]).map(x=>`<tr><td>${esc(x.engine)}</td><td>${esc(x.method)}</td><td>${esc(x.type)}</td><td>${badge(x.status,x.status==='supported'?'':'muted')}</td></tr>`).join('')}</tbody></table></div></section>`;}
  function renderSettings(){const s=state.settings||{};return`${title('系统设置','查看当前服务配置与敏感数据边界。')}<div class="overview-grid"><section class="panel"><div class="panel-header"><h2 class="panel-title">当前账号</h2></div><div class="settings-info"><p>管理员：${esc(state.user?.username)}</p><p>会话使用 HttpOnly、SameSite=Strict Cookie；变更请求要求同源确认头。</p><button class="btn" data-action="logout">退出登录</button></div></section><section class="panel"><div class="panel-header"><h2 class="panel-title">服务路径</h2></div><div class="detail-list settings-form"><div><span>数据目录</span><code>${esc(s.data_dir)}</code></div><div><span>默认备份目录</span><code>${esc(s.backup_root)}</code></div><div><span>主密钥文件（与 SQLite 分离）</span><code>${esc(s.master_key_path)}</code></div><div><span>备份超时</span><strong>${esc(s.backup_timeout)}</strong></div><div><span>服务启动时间</span><strong>${fmt(s.started_at)}</strong></div></div></section></div>`;}

  function render(){if(!state.user)return;document.querySelectorAll('[data-nav]').forEach(e=>e.classList.toggle('active',e.dataset.nav===state.page));$('#breadcrumb').textContent=pages[state.page];$('#nav-task-count').textContent=state.jobs.length;const renderers={overview:renderOverview,tasks:renderTasks,databases:renderDatabases,storage:renderStorage,logs:renderLogs,cleanup:renderCleanup,deployment:renderDeployment,settings:renderSettings};main.innerHTML=errorBanner()+(state.loading&&!state.jobs.length?`<div class="empty-state"><h3>正在读取真实服务数据…</h3></div>`:renderers[state.page]());}
  function navigate(page){if(!(page in pages))page='overview';if(location.hash!=='#'+page)location.hash=page;else{state.page=page;render();}document.body.classList.remove('nav-open');}
  // 弹窗只能通过“取消”或右上角关闭按钮退出：文本输入容易误触外部区域，点击遮罩不关闭。
  // 同时屏蔽 ESC（dialog 默认行为），避免未保存的表单内容被误清空。
  function openModal(t,sub,body,footer=''){modal.innerHTML=`<div class="modal-header"><div><h2>${esc(t)}</h2><p class="subtitle">${esc(sub)}</p></div><button class="btn btn-icon" data-action="close-modal" aria-label="关闭弹窗">${ico('x',20)}</button></div><div class="modal-body">${body}</div>${footer?`<div class="modal-footer">${footer}</div>`:''}`;if(!modal.open)modal.showModal();}
  const closeModal=()=>modal.close();
  function databaseModal(item){openModal(item?'编辑数据源':'添加数据源','连接测试返回真实版本；PostgreSQL 的维护库只用于连接，备份范围为整个实例；SQL Server 需先配置服务端可见的存储路径。',`<form id="database-form" data-id="${item?.id||''}" data-revision="${item?.revision||''}"><div class="form-grid"><label class="field">显示名称<input name="name" required maxlength="80" value="${esc(item?.name||'')}"></label><label class="field">数据库类型<select name="engine"><option value="mysql" ${item?.engine==='mysql'?'selected':''}>MySQL（逻辑全量）</option><option value="postgresql" ${item?.engine==='postgresql'?'selected':''}>PostgreSQL（整实例物理全量）</option><option value="sqlserver" ${item?.engine==='sqlserver'?'selected':''}>SQL Server（原生全量）</option></select></label><label class="field">主机地址<input name="host" required maxlength="255" value="${esc(item?.host||'127.0.0.1')}"></label><label class="field">端口<input name="port" type="number" min="1" max="65535" required value="${item?.port||(item?.engine==='postgresql'?5432:item?.engine==='sqlserver'?1433:3306)}"></label><label class="field">数据库 / 维护库<input name="database" required maxlength="64" value="${esc(item?.database||'')}"></label><label class="field">用户名<input name="username" maxlength="128" value="${esc(item?.username||'')}"></label><label class="field field-full">密码<input name="password" type="password" ${item?'':'required'} autocomplete="new-password"><span class="hint">只在当前表单提交，服务端加密保存；列表不会回显。</span></label><label class="field field-full" id="trust-cert-field" hidden>传输加密<div style="margin-top:6px"><label class="checkbox-row"><input type="checkbox" name="trust_server_certificate" ${item?.trust_server_certificate?'checked':''}> 信任服务器证书（不校验证书链）</label></div><span class="hint">仅 SQL Server 有效。自建实例通常使用自签名的实例证书，ODBC Driver 18 默认强制校验，不勾选会报 <code>certificate verify failed:self-signed certificate</code>。勾选后仍保留 TLS 加密，只跳过证书链校验；云上实例已配置受信证书时请保持不勾选。</span></label></div></form>`,`<button class="btn" data-action="close-modal">取消</button><button class="btn btn-primary" form="database-form">保存数据源</button>`);const form=document.querySelector('#database-form');if(form)updateTrustCertField(form);}
  function storageModal(item){openModal(item?'编辑本地存储':'添加本地存储','目录属于 DBBak 执行器所在主机；保存前执行真实写测试。',`<form id="location-form" data-id="${item?.id||''}" data-revision="${item?.revision||''}"><div class="form-grid"><label class="field">位置名称<input name="name" required maxlength="80" value="${esc(item?.name||'')}"></label><label class="field">存储类型<select name="kind" id="storage-kind"><option value="local" ${item?.kind==='local'||!item?'selected':''}>执行器本地目录</option><option value="unc" ${item?.kind==='unc'?'selected':''}>共享目录（SMB，填 UNC 路径）</option><option value="ftp" ${item?.kind==='ftp'?'selected':''}>FTP 远程存储</option><option value="oss" ${item?.kind==='oss'?'selected':''}>对象存储（阿里云 OSS / S3 兼容）</option></select></label><label class="field" id="ftp-host-field">FTP 主机<input name="host" maxlength="255" placeholder="例如 192.168.10.200" value="${esc(item?.host||'')}"></label><label class="field" id="ftp-port-field">FTP 端口<input name="port" type="number" min="1" max="65535" value="${item?.port||21}"></label><label class="field" id="ftp-user-field">FTP 账号<input name="ftp_username" maxlength="128" value="${esc(item?.kind==='ftp'?esc(item?.username||''):'')}"></label><label class="field" id="ftp-pass-field">FTP 密码<input name="ftp_password" type="password" autocomplete="new-password"><span class="hint">加密保存；编辑时留空表示保留原密码。</span></label><label class="field field-full" id="ftp-root-field">远程根目录<span class="hint">FTP 服务器上的绝对路径（如 /backups），保存时执行「上传-回读-删除」真实写检查。</span><input name="ftp_root" maxlength="300" placeholder="例如 /backups" value="${item?.kind==='ftp'?esc(item?.root||''):''}"></label><label class="field field-full" id="fs-root-field">授权根目录<span class="hint">执行器可访问的目录；共享目录填 UNC 路径（如 \\\\nas\\backup）。已有备份的目录不能改根路径；清理不能越出该目录。</span><input name="root" maxlength="300" value="${item?.kind==='ftp'?'':esc(item?.root||'')}"></label><label class="field field-full" id="srv-root-field">SQL Server 服务端路径（可选，属于此存储位置）<input name="server_root" maxlength="300" value="${esc(item?.server_root||'')}"><span class="hint">SQL Server 的 BACKUP DATABASE 写在实例本机，此路径必须是实例能看到的位置。当前只支持与执行器同一实际目录，两者不一致会在保存时被拒绝。</span></label><label class="field field-full" id="oss-endpoint-field">服务端点<span class="hint">只填主机名，不带 http:// 和路径。阿里云 OSS 形如 <code>oss-cn-hangzhou.aliyuncs.com</code>；同地域 ECS 可用内网端点 <code>oss-cn-hangzhou-internal.aliyuncs.com</code> 免公网流量费。</span><input name="endpoint" maxlength="200" placeholder="oss-cn-hangzhou.aliyuncs.com" value="${esc(item?.endpoint||'')}"></label><label class="field" id="oss-bucket-field">存储桶 Bucket<span class="hint">已存在且与 AccessKey 同账号。</span><input name="bucket" maxlength="63" placeholder="my-backup-bucket" value="${esc(item?.bucket||'')}"></label><label class="field" id="oss-region-field">地域 Region<span class="hint">如 cn-hangzhou；不确定时留空。</span><input name="region" maxlength="40" placeholder="cn-hangzhou" value="${esc(item?.region||'')}"></label><label class="field" id="oss-ak-field">AccessKey ID<input name="username" maxlength="128" autocomplete="off" value="${esc(item?.username||'')}"></label><label class="field" id="oss-sk-field">AccessKey Secret<span class="hint">加密保存；编辑时留空表示保留原 Secret。</span><input name="password" type="password" autocomplete="new-password"></label><label class="field field-full" id="oss-root-field">对象键前缀<span class="hint">可留空（直接写在桶根）。建议填 <code>backups</code>，实际落点为「前缀/子目录/数据库名/年/月/文件」。账号权限建议只授予该前缀：<code>s3:PutObject</code>、<code>s3:GetObject</code>、<code>s3:DeleteObject</code>、<code>s3:AbortMultipartUpload</code>、<code>s3:ListBucket</code>（带 <code>s3:prefix</code> 条件）。</span><input name="oss_root" maxlength="300" placeholder="backups" value="${item?.kind==='oss'?esc(item?.root||''):''}"></label><label class="field field-full" id="oss-options-field">连接选项<div style="display:flex;gap:18px;flex-wrap:wrap;margin-top:6px"><label class="checkbox-row"><input type="checkbox" name="use_ssl" ${item?.use_ssl===false?'':'checked'}> 使用 HTTPS</label><label class="checkbox-row"><input type="checkbox" name="path_style" ${item?.path_style?'checked':''}> Path-style 访问（MinIO 等私有部署勾选；阿里云 OSS 通常不勾）</label></div></label></div></form>`,`<button class="btn" data-action="close-modal">取消</button><button class="btn btn-primary" form="location-form">验证并保存</button>`);updateStorageFields();}
  function taskModal(item){
    if(!state.sources.length){toast('请先添加数据源。',true);return;}
    const c=item||{backup_type:'full',schedule_type:'daily',time_of_day:'02:00',weekday:0,interval_minutes:15,enabled:true,retention_days:30,min_full:1,source_id:state.sources[0].id,timezone:'Asia/Shanghai'};
    const defaultRoot=(item&&target(item.target_id)?.root)||state.settings?.backup_root||'';
    const logLabel=c.backup_method==='physical'?'binlog 归档（需物理全量基线）':'事务日志（需 FULL 恢复模式）';
    openModal(item?'编辑备份任务':'新建备份任务','按数据源选择备份路线，再填写落点与调度；保存时对目录执行真实写检查。',
      `<form id="task-form" data-id="${item?.id||''}" data-revision="${item?.revision||''}">
        <section class="form-section">
          <div class="form-section-title"><span class="number">01</span>基本信息</div>
          <div class="form-grid">
            <label class="field"><span class="field-label">任务名称</span><input name="name" required maxlength="100" placeholder="例如 生产库每日全量" value="${esc(c.name||'')}"></label>
            <label class="field"><span class="field-label">数据源</span><select name="source_id">${state.sources.map(x=>`<option value="${x.id}" ${x.id===c.source_id?'selected':''}>${esc(x.name)} · ${esc(x.database)}</option>`).join('')}</select></label>
          </div>
        </section>
        <section class="form-section">
          <div class="form-section-title"><span class="number">02</span>备份内容</div>
          <div class="form-grid">
            <label class="field" id="backup-method-field"><span class="field-label">备份路线</span><select name="backup_method"><option value="" ${!c.backup_method?'selected':''}>默认（逻辑全量）</option><option value="logical" ${c.backup_method==='logical'?'selected':''}>逻辑（mysqldump）</option><option value="physical" ${c.backup_method==='physical'?'selected':''}>物理（Percona XtraBackup）</option></select></label>
            <label class="field"><span class="field-label">备份类型</span><select name="backup_type" id="backup-type"><option value="full" ${c.backup_type==='differential'||c.backup_type==='log'?'':'selected'}>全量</option><option value="differential" ${c.backup_type==='differential'?'selected':''}>差异（需全量基线）</option><option value="log" ${c.backup_type==='log'?'selected':''}>${logLabel}</option></select></label>
            <div class="route-note" id="route-note"></div>
          </div>
        </section>
        <section class="form-section">
          <div class="form-section-title"><span class="number">03</span>备份落点</div>
          <div class="form-grid">
            <div class="field field-full" id="location-kind-field"><span class="field-label">备份位置类型</span>
              <div class="choice-row" id="location-kind-group">
                <label class="choice"><input type="radio" name="location_kind" value="storage" ${c.location_kind!=='database_server'?'checked':''}><span>存储位置（执行器目录）<small>写入执行器可达目录，保存时做真实写检查</small></span></label>
                <label class="choice"><input type="radio" name="location_kind" value="database_server" ${c.location_kind==='database_server'?'checked':''}><span>数据库服务器本地（仅 SQL Server）<small>文件不经过执行器，不做哈希校验，清理只清记录</small></span></label>
              </div>
            </div>
            <label class="field field-full" id="storage-field"><span class="field-label">本地存储目录</span><input name="storage_root" maxlength="300" value="${esc(defaultRoot)}"><span class="hint">执行器所在主机上的备份目录；相同目录的多个任务共用同一登记。</span></label>
            <label class="field field-full" id="server-dir-field"><span class="field-label">数据库服务器目录</span><input name="server_dir" maxlength="300" placeholder="例如 D:\\bak" value="${esc(c.server_dir||'')}"><span class="hint">SQL Server 所在主机上的绝对路径，目录需已存在且服务账号有写权限；执行器无法预先验证。</span></label>
            <label class="field field-full"><span class="field-label">输出子目录（可选）</span><input name="subdir" maxlength="120" placeholder="例如 mysql-prod 或 华东/mysql-prod" value="${esc(c.subdir||'')}"><span class="hint">相对存储根目录；实际落点为「根目录/子目录/数据库名/年/月/文件」，留空则直接写入根目录。</span></label>
          </div>
        </section>
        <section class="form-section">
          <div class="form-section-title"><span class="number">04</span>调度与保留</div>
          <div class="form-grid">
            <label class="field"><span class="field-label">调度类型</span><select name="schedule_type" id="schedule-type"><option value="daily" ${c.schedule_type==='daily'?'selected':''}>每日</option><option value="weekly" ${c.schedule_type==='weekly'?'selected':''}>每周</option><option value="interval" ${c.schedule_type==='interval'?'selected':''}>分钟间隔</option></select></label>
            <label class="field"><span class="field-label">时区</span><select name="timezone">${timezoneOptions(c.timezone)}</select><span class="hint">执行时间按此时区计算，数据库侧记录始终是 UTC。</span></label>
            <label class="field schedule-time"><span class="field-label">执行时间</span><input name="time_of_day" type="time" required value="${esc(c.time_of_day)}"></label>
            <label class="field schedule-weekday"><span class="field-label">星期</span><select name="weekday">${['周日','周一','周二','周三','周四','周五','周六'].map((l,i)=>`<option value="${i}" ${c.weekday===i?'selected':''}>${l}</option>`).join('')}</select></label>
            <label class="field schedule-interval"><span class="field-label">间隔（分钟）</span><input name="interval_minutes" type="number" min="1" max="1440" value="${c.interval_minutes}"></label>
            <label class="field"><span class="field-label">保留天数</span><input name="retention_days" type="number" min="1" max="3650" required value="${c.retention_days}"></label>
            <label class="field"><span class="field-label">最低全量份数</span><input name="min_full" type="number" min="1" max="100" required value="${c.min_full}"></label>
          </div>
          <div class="switch-list">
            <label class="switch-row"><input type="checkbox" name="enabled" ${c.enabled?'checked':''}><span><strong>启用后台调度</strong><small>关闭后保留配置，但不再自动执行</small></span></label>
            <label class="switch-row"><input type="checkbox" name="auto_cleanup" ${c.auto_cleanup?'checked':''}><span><strong>自动清理独立全量</strong><small>每天 05:00 后按保留策略执行，受保护的恢复点不动</small></span></label>
          </div>
          <p class="hint form-footnote">恢复链成员（binlog / WAL / 差异备份）暂不参与自动清理。</p>
        </section>
      </form>`,
      `<button class="btn" data-action="close-modal">取消</button><button class="btn btn-primary" form="task-form">保存任务</button>`);
    updateScheduleFields();updateBackupTypes();updateLocationFields();
  }
  // 路线决定备份语义，说明随选择切换，避免把前提条件堆在弹窗标题里。
  function routeStatus(engine,route){
    const caps=state.capabilities?.capabilities||[];
    const hit=caps.find(c=>c.engine===engine&&(c.route||'')===(route||'')&&c.type==='full');
    return hit?hit.status:'';
  }
  function routeNote(src,physical){
    if(!src)return {html:'',warn:false};
    const icon=ico('info',15);
    const flag=status=>{
      if(status==='unverified')return '<span class="route-flag">未验收</span>';
      if(status==='prerequisite_missing')return '<span class="route-flag">缺少工具</span>';
      return '';
    };
    if(src.engine==='mysql'){
      if(physical){
        const status=routeStatus('mysql','physical');
        return {html:`${icon}<div><strong>物理备份 · Percona XtraBackup${flag(status)}</strong>拷贝 InnoDB 数据页并追 redo 日志，适合大库；要求执行器与 MySQL 同机、XtraBackup 主版本与 MySQL 严格一致，且只能在 Linux 上运行。需设置 <code>DBBAK_MYSQL_PHYSICAL_ENABLED=1</code> 才会真正执行。</div>`,warn:!!status&&status!=='supported'};
      }
      const status=routeStatus('mysql','logical');
      return {html:`${icon}<div><strong>逻辑备份 · mysqldump${flag(status)}</strong>导出可读 SQL，可跨版本恢复、可只取单表；恢复需逐条重放，数据量越大越慢。几十 GB 以上的库建议改用物理路线。</div>`,warn:status==='prerequisite_missing'};
    }
    if(src.engine==='postgresql')return {html:`${icon}<div><strong>整实例物理全量 · pg_basebackup</strong>包含恢复所需的 WAL，粒度是整个实例而不是单个数据库；差异与日志链尚未实现。</div>`,warn:false};
    const status=routeStatus('sqlserver','native');
    return {html:`${icon}<div><strong>原生 BACKUP DATABASE${flag(status)}</strong>差异与事务日志需要已验证的全量基线，未完成验收前不建议开启。</div>`,warn:status==='prerequisite_missing'};
  }
  function updateBackupTypes(){
    const form=$('#task-form');if(!form)return;
    const select=form.elements.source_id,type=form.elements.backup_type,route=form.elements.backup_method;
    const src=(state.sources||[]).find(x=>x.id===select?.value);
    const isMySQL=src?.engine==='mysql';
    // 备份路线只对 MySQL 有意义：其他引擎各只有一条路线，隐藏后传空即默认。
    const routeField=form.querySelector('#backup-method-field');
    if(routeField)routeField.style.display=isMySQL?'':'none';
    if(route&&!isMySQL)route.value='';
    const physical=isMySQL&&route?.value==='physical';
    const allowDifferential=src?.engine==='sqlserver',allowLog=src?.engine==='sqlserver'||physical;
    Array.from(type.options).forEach(o=>{if(o.value==='differential')o.disabled=!allowDifferential;else if(o.value==='log')o.disabled=!allowLog;else o.disabled=false;});
    if(type.value==='differential'&&!allowDifferential)type.value='full';
    if(type.value==='log'&&!allowLog)type.value='full';
    // 日志备份在两条路线上语义不同：SQL Server 是事务日志，MySQL 物理路线是 binlog 归档。
    const logOption=Array.from(type.options).find(o=>o.value==='log');
    if(logOption)logOption.textContent=physical?'binlog 归档（需物理全量基线）':'事务日志（需 FULL 恢复模式）';
    const note=form.querySelector('#route-note');
    if(note){const r=routeNote(src,physical);note.className=r.warn?'route-note is-warn':'route-note';note.innerHTML=r.html;}
  }
  function updateLocationFields(){
    const form=$('#task-form');if(!form)return;
    const src=(state.sources||[]).find(x=>x.id===form.elements.source_id?.value);
    const allow=src?.engine==='sqlserver';
    const kindField=form.querySelector('#location-kind-field');
    const storageField=form.querySelector('#storage-field');
    const serverField=form.querySelector('#server-dir-field');
    if(!kindField||!storageField||!serverField)return;
    // 位置类型只有 SQL Server 有两种：其他引擎固定写执行器目录，隐藏整组而不是置灰。
    kindField.style.display=allow?'':'none';
    if(!allow)form.elements.location_kind.value='storage';
    const kind=form.elements.location_kind.value;
    const serverMode=allow&&kind==='database_server';
    storageField.style.display=serverMode?'none':'';
    serverField.style.display=serverMode?'':'none';
    form.elements.storage_root.required=!serverMode;
    form.elements.server_dir.required=serverMode;
    // 隐藏不等于不提交：切到数据库服务器本地后，之前填过的存储目录仍会随表单提交，
    // 而用户在界面上已看不到该字段、无法自行清除。这里同时做两件事——清空对侧字段值，
    // 并把隐藏字段置 disabled，使其不进 FormData；后端也按同一语义忽略残留值。
    if(serverMode)form.elements.storage_root.value='';
    else form.elements.server_dir.value='';
    form.elements.storage_root.disabled=serverMode;
    form.elements.server_dir.disabled=!serverMode;
    // 子目录只对存储位置有意义：数据库服务器本地目录是实例上的绝对路径，不存在二级子目录。
    const subField=form.elements.subdir?.closest('label');
    if(subField)subField.style.display=serverMode?'none':'';
  }
  // 证书信任只对 SQL Server 有意义；切到其他引擎时隐藏并清空勾选，避免留下无效配置。
  function updateTrustCertField(form){
    const field=form.querySelector('#trust-cert-field');
    const box=form.elements.trust_server_certificate;
    if(!field||!box)return;
    const isSQL=form.elements.engine.value==='sqlserver';
    field.hidden=!isSQL;
    if(!isSQL)box.checked=false;
  }
  function updateStorageFields(){
    const form=$('#location-form');if(!form)return;
    const kind=form.elements.kind?.value;if(!kind)return;
    const isFTP=kind==='ftp',isOSS=kind==='oss',isFS=kind==='local'||kind==='unc';
    // 三类落点各自独占一组字段：文件系统目录、FTP、对象存储。
    // 隐藏字段不清空会让服务端拿到互相矛盾的组合，因此这里同时控制 required。
    ['ftp-host-field','ftp-port-field','ftp-user-field','ftp-pass-field','ftp-root-field'].forEach(id=>{const el=form.querySelector('#'+id);if(el)el.style.display=isFTP?'':'none';});
    ['oss-endpoint-field','oss-bucket-field','oss-region-field','oss-ak-field','oss-sk-field','oss-root-field','oss-options-field'].forEach(id=>{const el=form.querySelector('#'+id);if(el)el.style.display=isOSS?'':'none';});
    const fsField=form.querySelector('#fs-root-field');if(fsField)fsField.style.display=isFS?'':'none';
    // SQL Server 服务端路径只对文件系统落点有意义；远程存储的文件不经执行器。
    const srvField=form.querySelector('#srv-root-field');if(srvField)srvField.style.display=isFS?'':'none';
    form.elements.root.required=isFS;
    form.elements.ftp_root.required=isFTP;
    form.elements.host.required=isFTP;
    form.elements.endpoint.required=isOSS;
    form.elements.bucket.required=isOSS;
    form.elements.oss_root.required=false;
    // AccessKey ID 在 FTP 与 OSS 之间复用同一个 name，两种模式都必须填。
    form.elements.ftp_username.required=isFTP;
    form.elements.ftp_password.required=false;
    form.elements.username.required=isOSS;
  }
  // 被隐藏的调度字段不能保持 required，否则表单会因看不见的空字段而无法提交。
  function updateScheduleFields(){const type=$('#schedule-type')?.value;document.querySelectorAll('.schedule-time').forEach(e=>{e.style.display=type==='interval'?'none':'';e.querySelectorAll('select,input').forEach(f=>f.required=type!=='interval');});document.querySelectorAll('.schedule-weekday').forEach(e=>{e.style.display=type==='weekly'?'':'none';e.querySelectorAll('select,input').forEach(f=>f.required=type==='weekly');});document.querySelectorAll('.schedule-interval').forEach(e=>{e.style.display=type==='interval'?'':'none';e.querySelectorAll('select,input').forEach(f=>f.required=type==='interval');});}  async function viewLog(id){try{const[run,logs]=await Promise.all([api(`/api/v1/runs/${id}`),api(`/api/v1/runs/${id}/logs`)]);openModal('执行详情',run.id,`<div class="detail-list"><div><span>状态</span>${runBadge(run)}</div><div><span>命令 / 文件 / 恢复</span><strong>${esc(run.command_status)} / ${esc(run.file_status)} / ${esc(run.restore_status)}</strong></div><div><span>开始</span><strong>${fmt(run.started_at)}</strong></div><div><span>结束</span><strong>${fmt(run.finished_at)}</strong></div>${run.error_message?`<div><span>失败原因</span><strong class="error-text">${esc(run.error_code)} · ${esc(run.error_message)}</strong></div>`:''}</div><pre class="run-log">${esc(logs.map(l=>`[${fmt(l.timestamp)}] ${l.level} ${l.message}`).join('\n')||'暂无日志')}</pre>`,`<button class="btn btn-primary" data-action="close-modal">关闭</button>`);}catch(error){toast(error.message,true);}}

  // 删除采用「先由服务端评估范围、再执行」的两步：预览展示真实文件清单与阻塞原因，
  // 确认时把同一目标名称回传，服务端重新评估后再逐个删除。范围不靠前端推算。
  async function removalPreview(kind,id,force=false){
    const urls={job:`/api/v1/jobs/${id}/removal`,files:`/api/v1/jobs/${id}/artifacts/removal`,artifact:`/api/v1/artifacts/${id}/removal`};
    return api(`${urls[kind]}${force?'?force=1':''}`);
  }
  function removalPlanHTML(plan,kind){
    const blockers=plan.blockers||[],files=plan.artifacts||[];
    // 单个对象的删除以文件路径标识；任务与任务文件以任务名标识，避免同名歧义。
    const label=kind==='artifact'?(plan.artifacts?.[0]?.relative_path||plan.job_name||'—'):(plan.job_name||'—');
    const rows=files.slice(0,12).map(a=>`<tr><td><span class="cell-title mono">${esc(a.relative_path)}</span></td><td>${bytes(a.size)}</td><td>${badge(a.state,a.state==='verified'?'':'warning')}${a.locked?badge('人工锁定','muted'):''}</td></tr>`).join('');
    return `<div class="detail-list"><div><span>操作对象</span><strong>${esc(label)}</strong></div>${kind!=='artifact'?`<div><span>所属任务</span><strong>${esc(plan.job_name||'—')}</strong></div>`:''}<div><span>备份文件</span><strong>${plan.file_count} 个 · ${bytes(plan.total_size)}</strong></div>${kind!=='artifact'?`<div><span>运行记录</span><strong>${plan.run_count} 条${(plan.active_runs||[]).length?` · ${plan.active_runs.length} 个未结束`:''}</strong></div>`:''}${kind==='job'?`<div><span>任务配置</span><strong>${plan.job_enabled?'将一并删除（需先暂停）':'将一并删除'}</strong></div>`:''}</div>${blockers.length?`<div class="notice error-banner">${ico('info')}<span><strong>当前无法直接删除：</strong>${blockers.map(b=>esc(b.message)+'（'+esc(b.remediation)+'）').join('；')}</span></div>`:''}${plan.warnings?.length?`<div class="notice">${ico('info')}<span>${plan.warnings.map(esc).join('；')}</span></div>`:''}${rows?`<div class="table-wrap"><table class="data-table"><thead><tr><th>文件</th><th>大小</th><th>状态</th></tr></thead><tbody>${rows}</tbody></table></div>${files.length>12?`<p class="hint">仅展示前 12 个对象，共 ${files.length} 个。</p>`:''}`:`<p class="hint">该范围内没有备份文件。</p>`}`;
  }
  function removalBody(planHTML,kind,id,force,confirmName){
    return `${planHTML}${force?`<div class="notice error-banner">${ico('trash')}<span><strong>此操作非常危险，可能导致不可逆的数据丢失！</strong>文件会从授权目录真实删除，记录不可恢复。请输入 <strong>${esc(confirmName)}</strong> 以确认。</span></div><label class="field">确认名称<input name="confirm_name" id="removal-confirm" autocomplete="off" placeholder="${esc(confirmName)}"></label>`:`<div class="notice">${ico('shield')}<span>当前为范围预览，不会删除任何内容。包含已校验备份时，需再勾选强制删除并输入名称确认。</span></div><label class="checkbox-row"><input type="checkbox" id="removal-force"> 我确认删除以上已校验备份与记录（强制删除）</label>`}<input type="hidden" id="removal-kind" value="${esc(kind)}"><input type="hidden" id="removal-id" value="${esc(id)}">`;
  }
  async function removalModal(kind,id,force=false){
    let plan;
    try{plan=await removalPreview(kind,id,force);}catch(error){toast(error.message,true);return;}
    state.removal={kind,id,plan,force};
    const isJob=kind==='job';
    const title=isJob?'删除备份任务':kind==='files'?'删除任务的全部备份文件':'删除备份文件';
    const sub=isJob?'先暂停任务，再删除其备份文件与配置；服务端会重新评估范围并逐个核对文件摘要。':kind==='files'?'仅删除任务产生的备份文件，任务配置与保留策略保留。':'删除单个恢复点；被后续差异或日志依赖的对象不会被删除。';
    openModal(title,sub,removalBody(removalPlanHTML(plan,kind),kind,id,force,kind==='artifact'?(plan.artifacts?.[0]?.relative_path||''):(plan.job_name||'')),
      `<button class="btn" data-action="close-modal">取消</button>${force?'':`<button class="btn btn-danger" data-action="removal-force" data-id="${esc(id)}">继续强制删除</button>`}${force?`<button class="btn btn-danger" data-action="execute-removal" data-id="${esc(id)}" ${plan.blockers?.length?'disabled':''}>确认删除</button>`:''}`);
  }
  async function executeRemoval(){
    const current=state.removal;if(!current)return;
    const confirm=current.force?(document.getElementById('removal-confirm')?.value||'').trim():'';
    const expected=current.kind==='job'?current.plan.job_name:(current.plan.artifacts?.[0]?.relative_path||'');
    if(current.force&&confirm!==expected){toast('确认名称与目标不一致。',true);return;}
    try{
      // 确认名称可能是中文任务名，只能放查询参数：fetch 的请求头限制 ISO-8859-1。
      const target=current.kind==='artifact'?`artifacts/${current.id}`:current.kind==='files'?`jobs/${current.id}/artifacts`:`jobs/${current.id}`;
      const query=current.force?`?force=1&confirm=${encodeURIComponent(confirm)}`:'';
      const result=await api(`/api/v1/${target}${query}`,{method:'DELETE'});
      closeModal();state.removal=null;state.cleanupPreview=null;
      if(state.cleanupJobID&&!state.jobs.some(x=>x.id===state.cleanupJobID)&&current.kind!=='artifact')state.cleanupJobID='';
      await loadAll({quiet:true});
      toast(result.job_deleted?'任务、备份文件与运行记录已删除。':`已删除 ${result.deleted_files} 个备份文件。`);
    }catch(error){
      // 部分失败时服务端返回 409 与逐文件结果，文件与记录保持原状。
      toast(`${error.message}`,true);
      if(error.status===409){await loadAll({quiet:true});removalModal(current.kind,current.id,current.force).catch(()=>{});}
    }
  }

  // 存储位置只解除登记，不动磁盘。引用检查与阻塞判定都由服务端给出，前端只负责展示与回传名称。
  async function storageRemovalModal(id){
    let plan;
    try{plan=await api(`/api/v1/storage-targets/${id}/removal`);}catch(error){toast(error.message,true);return;}
    state.storageRemoval=plan;
    const blockers=plan.blockers||[];
    const usage=plan.usage||{};
    const detail=`<div class="detail-list"><div><span>存储位置</span><strong>${esc(plan.name)}</strong></div><div><span>授权根目录</span><strong class="mono">${esc(plan.root)}</strong></div><div><span>关联任务</span><strong>${usage.job_count||0} 个</strong></div><div><span>备份对象</span><strong>${usage.artifact_count||0} 个</strong></div></div>`;
    const blockHTML=blockers.length?`<div class="notice error-banner">${ico('info')}<span><strong>当前无法删除：</strong>${blockers.map(b=>esc(b.message)+'（'+esc(b.remediation)+'）').join('；')}</span></div>`:'';
    const warnHTML=(plan.warnings||[]).length?`<div class="notice">${ico('shield')}<span>${plan.warnings.map(esc).join('；')}</span></div>`:'';
    const body=blockers.length?`${detail}${blockHTML}`:`${detail}${warnHTML}<div class="notice error-banner">${ico('trash')}<span><strong>此操作会移除该存储位置的登记，且不可撤销。</strong>磁盘上的文件不会被删除。请输入 <strong>${esc(plan.name)}</strong> 以确认。</span></div><label class="field">确认名称<input name="confirm_name" id="storage-confirm" autocomplete="off" placeholder="${esc(plan.name)}"></label>`;
    const footer=blockers.length
      ?`<button class="btn" data-action="close-modal">关闭</button>`
      :`<button class="btn" data-action="close-modal">取消</button><button class="btn btn-danger" data-action="execute-storage-delete" data-id="${esc(plan.id)}">确认删除</button>`;
    openModal('删除存储位置',blockers.length?'该存储位置仍被引用，需先解除引用。':'删除后不再出现在任务的可选存储位置中。',body,footer);
  }
  async function executeStorageDelete(id){
    const plan=state.storageRemoval;if(!plan||plan.id!==id)return;
    const confirm=(document.getElementById('storage-confirm')?.value||'').trim();
    if(confirm!==plan.name){toast('确认名称与存储位置不一致。',true);return;}
    try{
      // 名称可能含中文，只能走查询参数。
      await api(`/api/v1/storage-targets/${id}?confirm=${encodeURIComponent(confirm)}`,{method:'DELETE'});
      closeModal();state.storageRemoval=null;
      await loadAll({quiet:true});
      toast('存储位置登记已解除，磁盘文件未做改动。');
    }catch(error){toast(error.message,true);}
  }

  // 数据源删除只解除登记并清除加密凭据，不连接、不改动数据库本身。
  async function sourceRemovalModal(id){
    let plan;
    try{plan=await api(`/api/v1/sources/${id}/removal`);}catch(error){toast(error.message,true);return;}
    state.sourceRemoval=plan;
    const blockers=plan.blockers||[],usage=plan.usage||{};
    const engineLabel=plan.engine==='postgresql'?'PostgreSQL':plan.engine==='sqlserver'?'SQL Server':'MySQL';
    const detail=`<div class="detail-list"><div><span>数据源</span><strong>${esc(plan.name)}</strong></div><div><span>类型 / 地址</span><strong>${engineLabel} · ${esc(plan.endpoint)}</strong></div><div><span>数据库</span><strong>${esc(plan.database)}</strong></div><div><span>关联任务</span><strong>${usage.job_count||0} 个</strong></div><div><span>备份对象</span><strong>${usage.artifact_count||0} 个</strong></div></div>`;
    const blockHTML=blockers.length?`<div class="notice error-banner">${ico('info')}<span><strong>当前无法删除：</strong>${blockers.map(b=>esc(b.message)+'（'+esc(b.remediation)+'）').join('；')}</span></div>`:'';
    const warnHTML=`<div class="notice">${ico('shield')}<span>${(plan.warnings||[]).map(esc).join('；')}</span></div>`;
    const body=blockers.length?`${detail}${blockHTML}`:`${detail}${warnHTML}<div class="notice error-banner">${ico('trash')}<span><strong>此操作会删除数据源及其加密凭据，且不可撤销。</strong>数据库本身不受影响。请输入 <strong>${esc(plan.name)}</strong> 以确认。</span></div><label class="field">确认名称<input name="confirm_name" id="source-confirm" autocomplete="off" placeholder="${esc(plan.name)}"></label>`;
    const footer=blockers.length
      ?`<button class="btn" data-action="close-modal">关闭</button>`
      :`<button class="btn" data-action="close-modal">取消</button><button class="btn btn-danger" data-action="execute-source-delete" data-id="${esc(plan.id)}">确认删除</button>`;
    openModal('删除数据源',blockers.length?'该数据源仍被引用，需先解除引用。':'删除后连接信息与加密凭据不可恢复。',body,footer);
  }
  async function executeSourceDelete(id){
    const plan=state.sourceRemoval;if(!plan||plan.id!==id)return;
    const confirm=(document.getElementById('source-confirm')?.value||'').trim();
    if(confirm!==plan.name){toast('确认名称与数据源不一致。',true);return;}
    try{
      await api(`/api/v1/sources/${id}?confirm=${encodeURIComponent(confirm)}`,{method:'DELETE'});
      closeModal();state.sourceRemoval=null;
      await loadAll({quiet:true});
      toast('数据源与加密凭据已删除，数据库未受影响。');
    }catch(error){toast(error.message,true);}
  }

  async function action(element){const id=element.dataset.id,name=element.dataset.action;
    if(name==='new-task')return taskModal();if(name==='edit-task')return taskModal(job(id));if(name==='add-db')return databaseModal();if(name==='edit-db')return databaseModal(state.sources.find(x=>x.id===id));if(name==='add-location')return storageModal();if(name==='edit-location')return storageModal(state.targets.find(x=>x.id===id));if(name==='close-modal')return closeModal();if(name==='refresh')return loadAll();if(name==='view-log')return viewLog(id);if(name==='log-filter'){state.logFilter=element.dataset.value;return render();}
    if(name==='logout'){await api('/api/v1/auth/logout',{method:'POST',body:'{}'});return showLogin();}
    if(name==='run'){element.disabled=true;try{await api(`/api/v1/jobs/${id}/runs`,{method:'POST',body:'{}',headers:{'Idempotency-Key':`ui-${crypto.randomUUID()}`}});toast('已创建真实备份运行。');await loadAll({quiet:true});}catch(error){toast(error.message,true);element.disabled=false;}return;}
    if(name==='probe'){element.disabled=true;try{const r=await api(`/api/v1/sources/${id}/probe`,{method:'POST',body:'{}'});toast(`真实连接成功：${r.server_version}，连接测试通过。`);}catch(error){toast(`${error.code}: ${error.message}`,true);}finally{element.disabled=false;}return;}
    if(name==='check-storage'){try{await api(`/api/v1/storage-targets/${id}/check`,{method:'POST',body:'{}'});toast('存储位置真实写检查通过。');}catch(error){toast(error.message,true);}return;}
    if(name==='preview-cleanup'){if(!state.cleanupJobID)return;try{state.cleanupPreview=await api(`/api/v1/jobs/${state.cleanupJobID}/cleanup-previews`,{method:'POST',body:'{}'});render();toast('服务端清理预览已生成。');}catch(error){toast(error.message,true);}return;}
    if(name==='confirm-cleanup'){const candidates=state.cleanupPreview.result.items.filter(x=>x.candidate);return openModal('确认清理真实备份文件','此操作会删除授权目录中的已登记文件，请核对范围。',`<div class="notice">${ico('trash')}<span>将删除 ${candidates.length} 个文件，共 ${bytes(state.cleanupPreview.result.candidate_size)}。执行前服务端会再次校验预览、路径和 SHA-256。</span></div>`,`<button class="btn" data-action="close-modal">取消</button><button class="btn btn-danger" data-action="execute-cleanup">删除候选文件</button>`);}
    if(name==='execute-cleanup'){try{const r=await api('/api/v1/cleanup-runs',{method:'POST',body:JSON.stringify({preview_id:state.cleanupPreview.preview_id})});closeModal();state.cleanupPreview=null;await loadAll({quiet:true});toast(r.status==='success'?'候选备份已清理并登记。':'部分对象未删除，请检查执行结果。',r.status!=='success');}catch(error){toast(error.message,true);}return;}
    if(name==='hold'){try{await api(`/api/v1/artifacts/${id}/hold`,{method:'PATCH',body:JSON.stringify({locked:element.dataset.locked!=='true'})});state.cleanupPreview=null;await loadAll({quiet:true});}catch(error){toast(error.message,true);}return;}
    if(name==='delete-job')return removalModal('job',id);
    if(name==='delete-location')return storageRemovalModal(id);
    if(name==='execute-storage-delete')return executeStorageDelete(id);
    if(name==='delete-db')return sourceRemovalModal(id);
    if(name==='execute-source-delete')return executeSourceDelete(id);
    if(name==='delete-artifact')return removalModal('artifact',id);
    // 第二步：已看到范围预览后，勾选强制确认再按同目标重新评估，避免直接删除已校验备份。
    if(name==='removal-force'){
      const box=document.getElementById('removal-force');
      if(box&&!box.checked){toast('请先勾选强制删除确认框。',true);return;}
      const current=state.removal;if(!current)return;
      return removalModal(current.kind,current.id,true);
    }
    if(name==='execute-removal')return executeRemoval();
  }
  document.addEventListener('click',event=>{const nav=event.target.closest('[data-nav]');if(nav){event.preventDefault();navigate(nav.dataset.nav);return;}const element=event.target.closest('[data-action]');if(element){event.preventDefault();action(element).catch(error=>toast(error.message,true));}});
  document.addEventListener('input',event=>{if(event.target.id==='task-search'){state.search=event.target.value;$('#job-table').innerHTML=jobTable(filteredJobs());$('.toolbar-end').textContent=`共 ${filteredJobs().length} 个任务`;}});
  document.addEventListener('change',event=>{if(event.target.id==='schedule-type')updateScheduleFields();if(event.target.name==='source_id'&&event.target.closest('#task-form'))updateBackupTypes();if(event.target.name==='backup_method'&&event.target.closest('#task-form'))updateBackupTypes();if((event.target.name==='source_id'||event.target.name==='location_kind')&&event.target.closest('#task-form'))updateLocationFields();if(event.target.id==='storage-kind'&&event.target.closest('#location-form'))updateStorageFields();if(event.target.name==='engine'&&event.target.closest('#database-form')){const form=event.target.closest('#database-form');form.elements.port.value=event.target.value==='postgresql'?5432:event.target.value==='sqlserver'?1433:3306;updateTrustCertField(form);}if(event.target.id==='cleanup-job'){state.cleanupJobID=event.target.value;state.cleanupPreview=null;render();}});
  document.addEventListener('submit',async event=>{const form=event.target;event.preventDefault();if(!['login-form','database-form','location-form','task-form','retention-form'].includes(form.id))return;if(!form.reportValidity())return;const data=new FormData(form),value=n=>String(data.get(n)||'').trim();clearFormError();const submitBtn=form.closest('dialog')?.querySelector(`.modal-footer button[form="${form.id}"]`);if(submitBtn)submitBtn.disabled=true;try{
    if(form.id==='login-form'){const r=await api('/api/v1/auth/login',{method:'POST',body:JSON.stringify({username:value('username'),password:value('password')})});showApp(r.user);await loadAll();return;}
    if(form.id==='database-form'){const id=form.dataset.id,payload={name:value('name'),engine:value('engine'),host:value('host'),port:Number(value('port')),database:value('database'),username:value('username')},trust=form.elements.trust_server_certificate,password=String(data.get('password')||'');if(payload.engine==='sqlserver')payload.trust_server_certificate=!!trust?.checked;if(password||!id)payload.password=password;if(id)payload.revision=Number(form.dataset.revision);await api(id?`/api/v1/sources/${id}`:'/api/v1/sources',{method:id?'PATCH':'POST',body:JSON.stringify(payload)});closeModal();await loadAll({quiet:true});toast('数据源已保存，凭据已加密。');return;}
    if(form.id==='location-form'){
      const id=form.dataset.id,kind=value('kind')||'local';
      const isFTP=kind==='ftp',isOSS=kind==='oss';
      // 只提交与当前类型相关的字段：把 FTP 端口带进 OSS 会被服务端判为矛盾配置而拒绝。
      const payload={name:value('name'),kind};
      if(kind==='local'||kind==='unc'){payload.root=value('root');payload.server_root=value('server_root');}
      else if(isFTP){payload.root=value('ftp_root');payload.host=value('host');payload.port=Number(value('port')||21);payload.username=value('ftp_username');payload.passive=true;}
      else{payload.root=value('oss_root');payload.endpoint=value('endpoint');payload.bucket=value('bucket');payload.region=value('region');payload.username=value('username');payload.use_ssl=data.has('use_ssl');payload.path_style=data.has('path_style');}
      const secret=isFTP?value('ftp_password'):value('password');
      if(secret)payload.password=secret;
      if(id)payload.revision=Number(form.dataset.revision);
      await api(id?`/api/v1/storage-targets/${id}`:'/api/v1/storage-targets',{method:id?'PATCH':'POST',body:JSON.stringify(payload)});
      closeModal();await loadAll({quiet:true});
      toast(isOSS?'对象存储已通过真实写检查并保存。':'存储目录已通过真实写检查并保存。');
      return;
    }
    if(form.id==='task-form'){const id=form.dataset.id,payload={name:value('name'),source_id:value('source_id'),location_kind:value('location_kind')||'storage',server_dir:value('server_dir'),storage_root:value('storage_root'),subdir:value('subdir'),timezone:value('timezone')||'Asia/Shanghai',schedule_type:value('schedule_type'),time_of_day:value('time_of_day')||'02:00',weekday:Number(value('weekday')||0),interval_minutes:Number(value('interval_minutes')||15),enabled:data.has('enabled'),retention_days:Number(value('retention_days')),min_full:Number(value('min_full')),auto_cleanup:data.has('auto_cleanup'),backup_type:value('backup_type'),backup_method:value('backup_method')};if(id)payload.revision=Number(form.dataset.revision);await api(id?`/api/v1/jobs/${id}`:'/api/v1/jobs',{method:id?'PATCH':'POST',body:JSON.stringify(payload)});closeModal();await loadAll({quiet:true});toast('任务配置与下次执行时间已持久化。');return;}    if(form.id==='retention-form'){const current=job(state.cleanupJobID),payload={...current,retention_days:Number(value('retention_days')),min_full:Number(value('min_full')),auto_cleanup:current.auto_cleanup};await api(`/api/v1/jobs/${current.id}`,{method:'PATCH',body:JSON.stringify(payload)});state.cleanupPreview=null;await loadAll({quiet:true});toast('保留策略已保存，请重新预览。');}
  }catch(error){if(submitBtn)submitBtn.disabled=false;if(form.id==='login-form')$('#login-error').textContent=error.message;else showFormError(error);}});
  window.addEventListener('hashchange',()=>{if(!state.user)return;state.page=location.hash.slice(1) in pages?location.hash.slice(1):'overview';state.cleanupPreview=null;render();});
  // 禁止点击遮罩自动关闭；ESC 默认关闭行为一并拦截，弹窗只能显式关闭。
  modal.addEventListener('cancel',event=>event.preventDefault());
  modal.addEventListener('click',event=>{if(event.target===modal){const r=modal.getBoundingClientRect();if(event.clientX<r.left||event.clientX>r.right||event.clientY<r.top||event.clientY>r.bottom)event.preventDefault();}});
  setInterval(()=>{if(state.user&&state.runs.some(r=>['queued','running','verifying'].includes(r.state)))loadAll({quiet:true});},3000);
  api('/api/v1/auth/me').then(async user=>{showApp(user);await loadAll();}).catch(()=>showLogin());
})();
