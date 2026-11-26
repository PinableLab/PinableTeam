import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';

test('MCP import accepts common formats, validates bad input, and saves HTTP configuration', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const parsed = app.evaluate(`
    mcpImportCandidates(mcpParseJSONText(\`\\\`\\\`\\\`json
      {"mcpServers":{"docs":{"type":"streamable-http","url":"[Docs](https://mcp.example.test/api)",
      "headers":{"Authorization":"Bearer x"},"timeoutSec":90}}}
    \\\`\\\`\\\`\`))[0]
  `);
  assert.equal(parsed.id, 'docs');
  assert.equal(parsed.transport, 'http');
  assert.equal(parsed.url, 'https://mcp.example.test/api');
  assert.equal(JSON.stringify(parsed.headers), JSON.stringify({ Authorization: 'Bearer x' }));
  assert.throws(() => app.evaluate(`mcpNormalizeImportedServer({id:'bad',transport:'stdio'})`), /缺少 command/);
  assert.throws(() => app.evaluate(`mcpImportStringMap({A:{}},'headers')`), /必须是字符串/);

  app.router.post('/api/mcp-servers/custom', (call) => ({
    ...call.body.server, id: call.body.server.id, scope: call.body.scope, projectId: call.body.projectId,
  }));
  app.evaluate(`openCustomMCPModal();mcpApplyImportedServer(${JSON.stringify(parsed)})`);
  const form = app.document.querySelector('#customMcpForm');
  form.elements.scope.value = 'project';
  form.dispatchEvent(new app.window.Event('submit', { bubbles: true, cancelable: true }));
  await settle(8);
  const call = app.router.calls.find(c => c.path === '/api/mcp-servers/custom');
  assert.equal(call.method, 'POST');
  assert.equal(call.body.server.headers.Authorization, 'Bearer x');
  assert.equal(call.body.projectId, 'project-1');
});

test('generic SOP requirements reflect credentials and installed MCPs, then clone through API', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const generic = {
    id: 'generic-delivery', name: '通用交付', description: 'standard', generic: true, version: 2,
    nodes: [{ id: 'mr', name: '发起 MR', type: 'agent' }, { id: 'gate', name: '验收', type: 'approval' }],
    requirements: [
      { key: 'gongfeng-git-token', nodeId: 'mr' },
      { key: 'tapd-mcp', nodeId: 'mr' },
      { key: 'gongfeng-mcp', nodeId: 'mr' },
    ],
  };
  app.router.post('/api/workflows/generic-delivery/clone', (call) => ({
    ...generic, ...call.body, generic: false, tags: ['generic-source:generic-delivery'],
  }));
  let executors = [{ kind: 'codex', name: 'Codex', configured: true }];
  app.router.get('/api/workflows/generic-delivery/executor-requirements', () => executors);
  await app.evaluate(`
    state.currentUser={id:1,loginName:'owner',isPlatformAdmin:false};
    state.data.users=[{id:1,loginName:'owner',gitHttpCredentialsConfigured:true}];
    state.mcpMarket=[{id:'market-gf',name:'Gongfeng',aliases:['gongfeng'],url:'https://gongfeng'}];
    state.data.mcpServers=[{id:'gf',scope:'project',projectId:'project-1',marketId:'market-gf'}];
    state.data.workflows=[${JSON.stringify(generic)}];
    state.sopScope='generic';
    openGenericTemplateModal('generic-delivery');
  `);
  const rows = app.document.querySelector('#genericTemplateRequirements').textContent;
  assert.match(rows, /执行器 · Codex.*已配置/s);
  assert.doesNotMatch(rows, /CodeBuddy/);
  assert.match(rows, /Git 访问令牌.*已配置/s);
  assert.match(rows, /TAPD MCP.*未配置/s);
  assert.match(rows, /工蜂 MCP.*已配置/s);
  const sopHTML = app.evaluate(`({
    card:genericSOPCard(state.data.workflows[0]),
    chips:genericRequirementChips(state.data.workflows[0]),
    gongfeng:gongfengMCPInstalled(),
    tapd:tapdMCPInstalled(),
    node:workflowNodeName(state.data.workflows[0],'mr')
  })`);
  assert.match(sopHTML.card, /平台维护/);
  assert.doesNotMatch(sopHTML.card, /CodeBuddy/);
  assert.match(sopHTML.chips, /sop-badge tone-success[^>]*>.*Git 访问令牌/);
  assert.match(sopHTML.chips, /sop-badge tone-warn[^>]*>.*TAPD MCP/);
  assert.equal(sopHTML.gongfeng, true);
  assert.equal(sopHTML.tapd, false);
  assert.equal(sopHTML.node, '发起 MR');
  app.evaluate(`openGenericRequirementTarget('gongfeng-mcp')`);
  assert.equal(app.evaluate(`state.mcpQuery`), 'gongfeng');
  executors = [{ name: '', configured: false }];
  await app.evaluate(`openGenericTemplateModal('generic-delivery')`);
  assert.match(app.document.querySelector('#genericTemplateRequirements .req-row').textContent, /执行器.*未配置.*配置执行器/s);
  await app.evaluate(`adoptGenericTemplate()`);
  await settle(8);
  const clone = app.router.calls.find(c => c.path === '/api/workflows/generic-delivery/clone');
  assert.equal(clone.method, 'POST');
  assert.equal(clone.body.projectId, 'project-1');
  assert.equal(app.evaluate(`state.data.workflows[0].generic`), false);
});

test('runner cards expose runtime controls and persist limits, defaults, and auth mode', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const runner = {
    id: 'runner-local', name: 'Laptop', ownerUserId: 1, status: 'online', os: 'darwin', arch: 'arm64',
    runLimit: 2, lastSeenAt: new Date().toISOString(),
    runtimes: [{ provider: 'codex', status: 'online', authMode: 'local', version: '1.0' }],
  };
  app.router.put('/api/runners/runner-local/run-limit', { ...runner, runLimit: 5 });
  app.router.put('/api/runners/default', { message: 'default updated' });
  app.router.put('/api/runners/runtimes', { ok: true });
  app.router.delete('/api/runners/runner-local', { ok: true });
  app.router.post('/api/runners/bind-codes', { code: 'BIND123', expiresAt: '2099-01-01T00:00:00Z' });
  app.router.get('/api/runners', [runner]);
  app.router.get('/api/runners/default', { runnerId: '', cloudPool: { configured: true, online: 2 } });
  app.evaluate(`
    state.currentUser={id:1,loginName:'owner'};
    state.data.users=[{id:1,loginName:'owner',codexApiKeyConfigured:true}];
    state.runners=[${JSON.stringify(runner)}];state.defaultRunnerId='';state.cloudPool={configured:true,online:2};
    renderRunnersView();
  `);
  const list = app.document.querySelector('#runnersList');
  assert.match(list.textContent, /Laptop/);
  const limit = list.querySelector('[data-runner-run-limit]');
  assert.equal(limit.value, '2');
  assert.match(app.evaluate(`runnerConcurrencyLabel(state.runners[0])`), /最多同时运行 2 条 SOP/);
  limit.value = '5';
  limit.dispatchEvent(new app.window.Event('change', { bubbles: true }));
  await settle(6);
  assert.deepEqual(app.router.calls.find(c => c.path.endsWith('/run-limit')).body, { runLimit: 5 });
  await app.evaluate(`setDefaultRunner('runner-local');setRunnerRuntimeAuth('runner-local','codex','personal')`);
  await settle(6);
  assert.ok(app.router.calls.some(c => c.path === '/api/runners/default' && c.body.runnerId === 'runner-local'));
  assert.ok(app.router.calls.some(c => c.path === '/api/runners/runtimes' && c.body.authMode === 'personal'));
  await app.evaluate(`toggleRunnerRuntime('runner-local','codex',false)`);
  assert.ok(app.router.calls.some(c => c.path === '/api/runners/runtimes' && c.body.status === 'disabled'));
  app.evaluate(`window.__removeRunner=removeRunner('runner-local')`);
  await settle(2);
  app.document.querySelector('#confirmOkBtn').click();
  await app.evaluate(`window.__removeRunner`);
  assert.ok(app.router.calls.some(c => c.method === 'DELETE' && c.path === '/api/runners/runner-local'));
});

test('Desktop probe controls require an administrator and an online capable runtime', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const desktop = {provider:'pinable-desktop',status:'online',capabilities:['desktop.probe/v1']};
  const cli = {provider:'codex',status:'online'};
  const device = (id, runtimes, status='online') => ({id,ownerUserId:1,runtimes,status});
  const runners = [
    device('desktop',[desktop]), device('mixed',[desktop,cli]), device('older',[{...desktop,capabilities:[]}]),
    device('off',[desktop],'offline'), device('disabled',[{...desktop,status:'disabled'}]),
    device('cli',[{...cli,capabilities:['desktop.probe/v1']}]),
  ];
  app.evaluate(`state.authRequired=true;state.currentUser={id:1,isPlatformAdmin:false};state.runners=${JSON.stringify(runners)};renderRunnersView()`);
  const list = app.document.querySelector('#runnersList');
  assert.equal(list.querySelector('.runner-desktop-probe button'), null);
  await app.evaluate("runDesktopProbe('desktop')");
  assert.equal(app.router.calls.filter(c => c.path === '/api/runners/tasks').length, 0);
  app.evaluate("state.currentUser={id:1,isPlatformAdmin:true};renderRunnersView()");
  assert.deepEqual([...list.querySelectorAll('.runner-desktop-probe')].map(el => el.id), ['desktopProbe-desktop','desktopProbe-mixed']);
  for (const attr of ['data-runner-check-update','data-runner-run-limit','data-set-default']) {
    assert.equal(list.querySelector(`[${attr}="desktop"]`), null);
    assert.ok(list.querySelector(`[${attr}="mixed"]`));
  }
  app.evaluate("state.currentUser={isPlatformAdmin:true,isGuest:true};renderRunnersView()");
  assert.equal(list.querySelector('.runner-desktop-probe button'), null);
  app.evaluate("state.authRequired=false;renderRunnersView()");
  assert.ok(list.querySelector('#desktopProbe-desktop button'));
});

test('Desktop probe tracks its targeted task and resumes status checks without dispatching again', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const ticks = [];
  const timeout = app.window.setTimeout.bind(app.window);
  app.window.setTimeout = (fn,ms,...args) => ms === 2000 ? ticks.push(fn) : timeout(fn,ms,...args);
  const advance = async () => {assert.equal(ticks.length,1);ticks.shift()();await settle(1)};
  app.evaluate(`state.authRequired=true;state.currentUser={id:1,isPlatformAdmin:true};
    state.runners=[{id:'desk',ownerUserId:1,status:'online',runtimes:[
      {provider:'pinable-desktop',status:'online',capabilities:['desktop.probe/v1']}]}];renderRunnersView()`);
  const task = {id:'rt-demo',status:'queued'};
  app.router.post('/api/runners/tasks', task);
  app.router.get('/api/runners/tasks/rt-demo/status', () => ({...task,result:'private result',error:'private error'}));
  const row = () => app.document.getElementById('desktopProbe-desk');
  row().querySelector('button').click();
  await settle(2);
  assert.deepEqual(app.router.calls.find(c => c.method === 'POST' && c.path === '/api/runners/tasks').body,
    {runnerId:'desk',provider:'pinable-desktop',action:'desktop_probe_v1'});
  assert.match(row().textContent,/rt-demo · 已排队/);
  assert.equal(row().querySelector('button').disabled,true);
  await app.evaluate("runDesktopProbe('desk')");
  task.status = 'running';
  await advance();
  assert.match(row().textContent,/rt-demo · 验证中/);
  app.evaluate("state.runners[0].status='offline';renderRunnersView()");
  for(let i=1;i<30;i++)await advance();
  assert.match(row().textContent,/自动刷新已暂停/);
  assert.equal(row().querySelector('button').textContent,'刷新验证状态');
  task.status = 'success';
  row().querySelector('button').click();
  await advance();
  assert.match(row().textContent,/rt-demo · 验证成功/);
  assert.doesNotMatch(row().textContent,/private/);
  assert.equal(row().querySelector('button').hidden,true);
  assert.equal(app.router.calls.filter(c => c.method === 'POST' && c.path === '/api/runners/tasks').length,1);
  assert.equal(ticks.length,0);
});

test('Desktop pairing issues fresh one-use codes and copies only the selected value', async (t) => {
  const app = await loadApp({origin:'https://work.example.test'});
  t.after(app.close);
  const field = id => app.document.getElementById(id);
  const copied = [];
  app.window.navigator.clipboard.writeText = async text => { copied.push(text) };
  let issued = 0;
  app.router.post('/api/runners/bind-codes', () => ({code:'bind-'+(++issued),expiresAt:'2099-01-01T00:00:00Z'}));
  app.evaluate(`state.authRequired=true;state.currentUser={id:2};state.runnerBindCode={code:'existing-cli'};
    renderRunnersView()`);
  assert.equal(field('desktopPairAccessNote').hidden,false);
  assert.equal(field('copyDesktopPairCode').hidden,true);
  field('copyDesktopOrigin').click();
  field('newDesktopPairCode').click();
  field('newDesktopPairCode').click();
  await settle();
  assert.equal(issued,1);
  assert.equal(field('desktopPairCode').textContent,'bind-1');
  field('copyDesktopPairCode').click();
  assert.deepEqual(copied,['https://work.example.test','bind-1']);
  field('newDesktopPairCode').click();
  await settle();
  assert.equal(field('desktopPairCode').textContent,'bind-2');
  assert.equal(field('bindToken').textContent,'existing-cli');
  assert.doesNotMatch(field('desktopPairInstructions').textContent,/bind-2/);
  app.evaluate('Date.now=()=>Date.parse("2100-01-01T00:00:00Z")');
  field('copyDesktopPairCode').click();
  assert.equal(field('copyDesktopPairCode').hidden,true);
  assert.match(field('desktopPairCode').textContent,/已过期/);
  assert.equal(copied.length,2);
  app.evaluate('state.currentUser={id:3,isGuest:true};renderRunnersView()');
  assert.equal(field('desktopPairing').hidden,true);
  assert.equal(field('desktopPairCode').textContent,'尚未生成');
});

test('an owner can unpair an online Desktop independently of its latest runtime advertisement', async (t) => {
  const app = await loadApp();
  t.after(app.close);
  const device = (id,ownerUserId,status) => ({id,name:id,ownerUserId,status,runtimes:[]});
  const runners = [device('runner-desktop-my',7,'online'),device('runner-desktop-off',7,'offline'),
    device('runner-desktop-other',8,'online'),device('runner-cli-my',7,'online')];
  app.router.get('/api/runners',runners.slice(1));
  app.router.delete('/api/runners/runner-desktop-my',{ok:true});
  app.evaluate(`state.authRequired=true;state.currentUser={id:7,isPlatformAdmin:false};
    state.runners=${JSON.stringify(runners)};renderRunnersView()`);
  const remove = id => app.document.querySelector(`[data-remove-runner="${id}"]`);
  assert.ok(remove('runner-desktop-off'));
  assert.equal(remove('runner-desktop-other'),null);
  assert.equal(remove('runner-cli-my'),null);
  assert.equal(remove('runner-desktop-my').textContent,'解除配对');
  assert.equal(app.document.querySelector('[data-runner-check-update="runner-desktop-my"]'),null);
  remove('runner-desktop-my').click();
  await settle();
  assert.match(app.document.getElementById('confirmMessage').textContent,/凭据将失效/);
  app.document.getElementById('confirmCancelBtn').click();
  await settle();
  assert.ok(remove('runner-desktop-my'));
  assert.equal(app.router.calls.filter(c => c.method === 'DELETE').length,0);
  remove('runner-desktop-my').click();
  await settle();
  app.document.getElementById('confirmOkBtn').click();
  await settle(8);
  assert.equal(app.router.calls.filter(c => c.method === 'DELETE' && c.path === '/api/runners/runner-desktop-my').length,1);
  assert.equal(remove('runner-desktop-my'),null);
});
