// Requires Node 20+, Playwright and Chromium. Uses a temporary QA user/key.
const {chromium,request}=require('playwright');
(async()=>{
 const base=process.env.TEST_URL||'http://127.0.0.1:8000';
 const admin=process.env.ADMIN_PASSWORD;const adminUsername=process.env.ADMIN_USERNAME||'admin';if(!admin)throw Error('ADMIN_PASSWORD required');
 const browser=await chromium.launch({headless:true,args:['--no-sandbox']});
 const adminContext=await request.newContext();
 const signedIn=await adminContext.post(base+'/auth/login',{headers:{'X-Model-Meter':'dashboard'},data:{username:adminUsername,password:admin}});if(!signedIn.ok())throw Error('Admin sign-in failed');
 const context=await browser.newContext({viewport:{width:1440,height:1100}});
 const page=await context.newPage();const errors=[];const reads=[];
 const tag=Date.now().toString();let userID,keyID,liveConnections=0;
 page.on('pageerror',e=>errors.push(e.message));
 page.on('request',r=>{if(r.url().includes('/admin/events'))liveConnections++;if(r.url().includes('/admin/')&&r.method()==='GET'&&!r.url().includes('/admin/events'))reads.push(r.url())});
 const adminAPI=async(path,body)=>{const r=await adminContext.fetch(base+'/admin/'+path,{method:body?'POST':'GET',headers:{'X-Model-Meter':'dashboard',...(body?{'Content-Type':'application/json'}:{})},data:body});if(!r.ok())throw Error('Admin cleanup/setup request failed: '+r.status());return r.json()};
 const login=async(username,password)=>{await page.locator('#loginUsername').fill(username);await page.locator('#adminToken').fill(password);await page.locator('#loginForm button').click();await page.locator('#dashboard').waitFor({state:'visible'});await page.waitForFunction(()=>document.getElementById('connectionState').textContent==='LIVE')};
 const view=async(name)=>page.locator('[data-view="'+name+'"]').click();
 try{
  await page.goto(base);await login(adminUsername,admin);
  const cookie=(await context.cookies()).find(c=>c.httpOnly);if(!cookie||cookie.sameSite!=='Strict'||cookie.expires<Date.now()/1000)throw Error('persistent secure session attributes missing');
  if(await page.evaluate(secret=>[...Object.values(localStorage),...Object.values(sessionStorage)].some(v=>v.includes(secret)),admin))throw Error('unexpected local storage credential');
  await page.reload();await page.locator('#dashboard').waitFor({state:'visible'});
  for(const minutes of [1,5,15,60,240,720]){
   const options=await page.locator('#period option').evaluateAll(opts=>opts.map(o=>Number(o.value)));
   const match=options.find(v=>Math.abs(v*60-minutes)<0.00001);if(match===undefined)throw Error('Missing window '+minutes);
   await page.selectOption('#period',String(match));
  }
  await page.selectOption('#period','24');
  if(await page.locator('#adminToken').getAttribute('autocomplete')!=='current-password'||await page.locator('#adminToken').getAttribute('name')!=='password')throw Error('password autofill metadata missing');
  if(await page.locator('#loginForm input[autocomplete="username"]').count()!==1)throw Error('username autofill target missing');
  await view('access');
  await page.locator('#newUsername').fill('browser-qa-'+tag);await page.locator('#userName').fill('Browser QA '+tag);await page.locator('#userForm button').click();await page.locator('#userSecret').waitFor({state:'visible'});const userToken=await page.locator('#userToken').textContent();await page.locator('#dismissUser').click();
  userID=(await adminAPI('users')).find(u=>u.name==='Browser QA '+tag).id;
  await view('overview');if(await page.locator('#view-overview svg').count()!==5)throw Error('charts missing');await page.screenshot({path:'/tmp/model-meter-desktop.png',fullPage:true});
  await page.locator('#logout').click();await page.locator('#login').waitFor({state:'visible'});await page.reload();await page.locator('#login').waitFor({state:'visible'});
  if((await context.request.get(base+'/admin/me')).status()!==401)throw Error('logout survived as active session');
  await login('browser-qa-'+tag,userToken);await page.reload();await page.locator('#dashboard').waitFor({state:'visible'});await view('access');if(await page.locator('#usersPanel').isVisible())throw Error('user management visible to regular user');
  await page.locator('#keyName').fill('Browser QA key '+tag);await page.locator('#createForm button').click();await page.locator('#secret').waitFor({state:'visible'});const key=await page.locator('#newKey').textContent();await page.locator('#dismiss').click();keyID=(await adminAPI('keys')).find(k=>k.name==='Browser QA key '+tag).id;
  await page.locator('#profileName').fill('Browser QA renamed '+tag);await page.locator('#profileForm button').click();await page.waitForFunction(()=>document.getElementById('message').textContent==='Profile saved.');
  await view('overview');await page.waitForTimeout(600);const before=await page.locator('#requests').textContent();const started=Date.now();
  const result=await context.request.get(base+'/v1/models',{headers:{Authorization:'Bearer '+key}});if(result.status()!==200)throw Error('user key failed');await result.body();
  await page.waitForFunction(old=>document.getElementById('requests').textContent!==old,before,{timeout:3000});const liveLatency=Date.now()-started;
  await view('requests');await page.locator('#requestRows tr').filter({hasText:'/v1/models'}).waitFor();await view('overview');
  // More than one heartbeat interval: there must be no periodic HTTP data reads.
  await page.waitForTimeout(500);const readCount=reads.length;await page.waitForTimeout(16200);const idleRequests=reads.length-readCount;if(idleRequests!==0)throw Error('dashboard polled while idle');
  const connectionsBefore=liveConnections;await context.setOffline(true);await page.waitForFunction(()=>document.getElementById('connectionState').textContent==='RECONNECTING',{},{timeout:3000});await context.setOffline(false);await page.waitForFunction(()=>document.getElementById('connectionState').textContent==='LIVE',{},{timeout:15000});if(liveConnections<=connectionsBefore)throw Error('live connection was not re-established');
  // An external configuration change must reach this user's already-open UI.
  await adminAPI('key-update',{id:keyID,name:'Live rename '+tag});await view('access');await page.locator('#keyRows tr').filter({hasText:'Live rename '+tag}).waitFor({timeout:3000});
  await page.setViewportSize({width:390,height:844});await view('overview');await page.screenshot({path:'/tmp/model-meter-mobile.png',fullPage:true});if(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth))throw Error('mobile overflow');
  await view('access');page.on('dialog',d=>d.accept());await page.locator('#keyRows button.danger').click();await page.waitForFunction(()=>!document.querySelector('#keyRows button.danger'));const revoked=await context.request.get(base+'/v1/models',{headers:{Authorization:'Bearer '+key}});if(revoked.status()!==401)throw Error('revoked key accepted');
  await adminAPI('users/update',{id:userID,admin:true});await page.locator('#login').waitFor({state:'visible'});await login('browser-qa-'+tag,userToken);await view('access');
  if(!await page.locator('#usersPanel').isVisible())throw Error('named administrator management unavailable');
  if(!(await page.locator('#identityName').textContent()).includes('Browser QA renamed'))throw Error('named administrator identity missing');
  const replacement='Browser-password-'+tag;await page.locator('#currentPassword').fill(userToken);await page.locator('#newPassword').fill(replacement);await page.locator('#passwordForm button').click();await page.locator('#login').waitFor({state:'visible'});await login('browser-qa-'+tag,replacement);
  await adminAPI('users/update',{id:userID,disabled:true});await page.locator('#login').waitFor({state:'visible',timeout:3000});if(await page.locator('#dashboard').isVisible())throw Error('disabled session remained open');
  if(errors.length)throw Error(errors.join('; '));console.log(JSON.stringify({result:'passed',live_update_ms:liveLatency,idle_data_requests:idleRequests,checks:['named administrator role','own password change','persistent admin and user sessions','logout then reload','password-manager field semantics','requested window options','both roles','user/key creation','profile edit','SSE update','no idle polling for 16 seconds','network reconnect','external key rename','live session revocation','mobile layout','charts']}));
 }finally{
  if(keyID)await adminAPI('revoke',{id:keyID}).catch(()=>{});
  if(userID)await adminAPI('users/update',{id:userID,disabled:true}).catch(()=>{});
  await adminContext.post(base+'/auth/logout',{headers:{'X-Model-Meter':'dashboard'},data:{}}).catch(()=>{});await adminContext.dispose();await browser.close();
 }
})().catch(e=>{console.error(e);process.exit(1)});
