import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import {JSDOM} from 'jsdom';
const source=readFileSync(new URL('../../web/app/bots.js',import.meta.url),'utf8');
const png='data:image/png;base64,iVBORw0KGgoAAA==';
const ticket=()=>({id:'ticket1',qrImageDataUri:png,status:'pending',expiresAt:new Date(Date.now()+300000).toISOString(),intervalSec:5});
function fixture(){
 const dom=new JSDOM('<button id="botNew"></button><div id="botsContent"></div>');
 const calls=[],timers=new Map();let handler=()=>ticket(),timerId=0;
 const context=vm.createContext({window:dom.window,document:dom.window.document,state:{projectFilter:'p1',currentUser:{id:1}},$:s=>dom.window.document.querySelector(s),escapeHTML:v=>String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),setTimeout:fn=>{timers.set(++timerId,fn);return timerId},clearTimeout:id=>timers.delete(id),structuredClone,Date,console,requestAnimationFrame:()=>{},toast:()=>{},nodeBotCache:new Map(),api:async(path,opts={})=>{calls.push({path,...opts});return handler(path,opts)},isPlatformAdmin:()=>true,currentProject:()=>({}),getComputedStyle:dom.window.getComputedStyle});
 vm.runInContext(source,context);
 vm.runInContext("botUI.projectId='p1';botUI.tab='edit';botUI.selected={name:'通知',channel:'feishu',enabled:false};applyBotMotion=()=>{};renderBots()",context);
 return {dom,calls,timers,run:code=>vm.runInContext(code,context),api:fn=>handler=fn};
}
test('new Feishu defaults QR first; no secret fields and unsafe image rejected',async()=>{
 const f=fixture();assert.equal(f.dom.window.document.querySelector('[name="secret"]'),null);
 assert.ok(f.dom.window.document.querySelector('[data-feishu-qr="start"]'));
 await f.run("feishuQRAction('start')");
 assert.equal(f.dom.window.document.querySelector('img').src,png);
 assert.equal(f.dom.window.document.querySelector('[data-feishu-qr="save"]').disabled,true);
 assert.equal(JSON.parse(f.calls[0].body).name,'通知');
 assert.equal(f.run("feishuQRPNG('https://evil.invalid/image')"),'');
 assert.equal(f.run("feishuQRPNG('data:image/svg+xml;base64,abc')"),'');
});
test('ready scan never saves automatically; explicit save consumes and selects disabled bot',async()=>{
 const f=fixture();
 f.api(path=>path.endsWith('/consume')?{id:'b1'}:path.startsWith('/api/notification-bots?')?[{id:'b1',projectId:'p1',channel:'feishu',name:'通知',enabled:false,interactionEnabled:false}]:path.endsWith('/poll')?{...ticket(),status:'ready',appId:'cli_safe'}:ticket());
 await f.run("feishuQRAction('start')");await f.run("feishuQRAction('poll')");
 assert.equal(f.calls.filter(x=>x.path.endsWith('/consume')).length,0);
 await f.run("feishuQRAction('save')");
 assert.equal(f.run('botUI.selected.id'),'b1');assert.equal(f.run('botUI.selected.enabled'),false);
 assert.equal(f.run('botUI.step'),2);assert.equal(f.run('feishuQR.ticket'),null);
 assert.deepEqual(JSON.parse(f.calls.find(x=>x.path.endsWith('/consume')).body),{projectId:'p1'});
 assert.equal(f.calls.some(x=>x.path.endsWith('/test')),false);
});
test('cancel deletes pending ticket; failed cancellation retains retry state',async()=>{
 const f=fixture();await f.run("feishuQRAction('start')");
 f.api(()=>{throw Error('cancel failed')});await f.run("feishuQRAction('cancel')");
 assert.equal(f.run('feishuQR.ticket.id'),'ticket1');
 assert.match(f.dom.window.document.querySelector('[role="alert"]').textContent,/cancel failed/);
 f.api(()=>undefined);await f.run("feishuQRAction('cancel')");
 assert.equal(f.run('feishuQR.ticket'),null);assert.equal(f.calls.at(-1).method,'DELETE');
});
test('expired and denied tickets cannot be consumed; server expiry enables a new start',async()=>{
 const f=fixture();await f.run("feishuQRAction('start')");
 f.run("feishuQR.ticket.status='ready';feishuQR.ticket.expiresAt='2000-01-01T00:00:00Z';renderBots()");
 await f.run("feishuQRAction('save')");assert.equal(f.calls.length,1);
 f.run("feishuQR.ticket.expiresAt=new Date(Date.now()+10000).toISOString();feishuQR.ticket.status='denied'");
 await f.run("feishuQRAction('save')");assert.equal(f.calls.length,1);
 f.api(()=>{throw Object.assign(Error('expired'),{status:410})});
 await f.run("feishuQRAction('poll')");assert.equal(f.run('feishuQR.ticket'),null);
});
test('late create after editor/project change is cancelled and never painted',async()=>{
 const f=fixture();let resolve;f.api(()=>new Promise(r=>resolve=r));
 const pending=f.run("feishuQRAction('start')");
 f.run("feishuQRReset();state.projectFilter='p2';botUI.projectId='p2'");
 f.api(()=>undefined);resolve(ticket());await pending;
 assert.equal(f.run('feishuQR.ticket'),null);
 assert.ok(f.calls.some(x=>x.method==='DELETE'&&x.path.endsWith('?projectId=p1')));
});
test('advanced fallback preserves manual setup but does not enable the new bot',async()=>{
 const f=fixture();await f.run("feishuQRAction('manual')");
 assert.ok(f.dom.window.document.querySelector('[name="secret"]'));
 assert.equal(f.dom.window.document.querySelector('[name="enabled"]').checked,false);
 assert.equal(f.dom.window.document.querySelector('[name="interactionEnabled"]').disabled,true);
});
test('busy duplicate clicks create only one ticket; malformed poll is not accepted',async()=>{
 const f=fixture();let resolve;f.api(()=>new Promise(r=>resolve=r));
 const p=f.run("feishuQRAction('start')");await f.run("feishuQRAction('start')");
 assert.equal(f.calls.length,1);resolve(ticket());await p;
 f.api(()=>({...ticket(),id:'other'}));await f.run("feishuQRAction('poll')");
 assert.equal(f.run('feishuQR.ticket.id'),'ticket1');
 assert.match(f.run('feishuQR.error'),/无效/);
});
test('automatic polling remains bounded and stops on leaving the editor',async()=>{
 const f=fixture();await f.run("feishuQRAction('start')");
 assert.equal(f.timers.size,1);
 const next=[...f.timers.values()][0];f.timers.clear();next();
 await new Promise(resolve=>setImmediate(resolve));
 assert.ok(f.calls.some(c=>c.path.endsWith('/poll')));
 f.run("botUI.tab='list'");
 const timer=[...f.timers.values()][0];f.timers.clear();timer();
 await new Promise(resolve=>setImmediate(resolve));
 assert.equal(f.run('feishuQR.ticket'),null);assert.equal(f.calls.at(-1).method,'DELETE');
});
test('return from advanced clears unsaved credentials and restores QR-first editor',async()=>{
 const f=fixture();await f.run("feishuQRAction('manual')");
 f.run("botUI.selected._secret='fixture-secret';botUI.selected.botId='cli_test'");
 await f.run("feishuQRAction('qr')");
 assert.equal(f.run('botUI.selected._secret'),'');
 assert.equal(f.dom.window.document.querySelector('[name="secret"]'),null);
 assert.ok(f.dom.window.document.querySelector('[data-feishu-qr="start"]'));
});
