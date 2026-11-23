const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const source=fs.readFileSync(__dirname+'/../web/app/bots.js','utf8');
function extract(name){const start=source.indexOf('function '+name+'(');assert(start>=0);const next=source.indexOf('\nfunction ',start+1);return source.slice(start,next<0?undefined:next)}
const ctx={feishuQR:{manual:true},botUI:{busy:false,query:''},escapeHTML:v=>String(v??'').replaceAll('<','&lt;').replaceAll('"','&quot;'),botBusyLabel:()=> '保存',botModelPickerHTML:()=>'<input name="model">'};
vm.createContext(ctx);
for(const name of ['botChannel','botCredentialPaneHTML','captureBotDraft'])vm.runInContext(extract(name),ctx);
test('Manual Feishu credentials remain explicitly outbound-only',()=>{
 const html=ctx.botCredentialPaneHTML({channel:'feishu',name:'n',botId:'cli_test',enabled:true},true);
 assert.match(html,/>App ID</);assert.match(html,/>App Secret</);assert.match(html,/name="interactionEnabled"[^>]*disabled/);
 assert.match(html,/不接收消息或执行对话/);assert.match(html,/name="channel"/);
});
test('Legacy bot remains WeCom and channel immutable after save',()=>{
 const html=ctx.botCredentialPaneHTML({id:'bot-1',name:'n',botId:'old'},true);
 assert.match(html,/>Bot ID</);assert.match(html,/name="channel" disabled/);
 assert.doesNotMatch(html,/name="interactionEnabled"[^>]*disabled/);
});
test('Feishu draft cannot retain automatic execution checkbox',()=>{
 ctx.botUI.selected={}; const elements={};
 for(const [k,value] of Object.entries({channel:'feishu',name:'n',botId:'cli',model:'default',secret:'fake'}))elements[k]={value};
 elements.enabled={checked:true};elements.interactionEnabled={checked:true};
 ctx.captureBotDraft({querySelector:()=>({elements})});
 assert.equal(ctx.botUI.selected.channel,'feishu');assert.equal(ctx.botUI.selected.interactionEnabled,false);
});
