// Run with NODE_PATH pointing to an installed Playwright package; install Chromium first.
const {chromium}=require('playwright');
const fs=require('node:fs');
(async()=>{
 const base=process.env.TEST_URL||'http://127.0.0.1:8001';
 const admin=fs.readFileSync(process.env.ADMIN_TOKEN_FILE||'data/admin-token','utf8').trim();
 const browser=await chromium.launch({headless:true,args:['--no-sandbox']});
 const page=await browser.newPage({viewport:{width:1440,height:1200}});
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 try{
 await page.goto(base);await page.locator('#adminToken').fill(admin);await page.locator('#loginForm button').click();await page.locator('#dashboard').waitFor({state:'visible'});
 await page.locator('#userName').fill('Browser QA user');await page.locator('#userForm button').click();await page.locator('#userSecret').waitFor({state:'visible'});const userToken=await page.locator('#userToken').textContent();await page.locator('#dismissUser').click();
 await page.locator('#userRows tr').filter({hasText:'Browser QA user'}).waitFor();
 if(await page.locator('.charts svg').count()!==2)throw Error('charts missing');
 await page.screenshot({path:'/tmp/model-meter-desktop.png',fullPage:true});
 await page.locator('#logout').click();await page.locator('#adminToken').fill(userToken);await page.locator('#loginForm button').click();await page.locator('#dashboard').waitFor({state:'visible'});
 if(await page.locator('#usersPanel').isVisible())throw Error('user management visible to regular user');
 await page.locator('#keyName').fill('Browser QA key');await page.locator('#createForm button').click();await page.locator('#secret').waitFor({state:'visible'});const key=await page.locator('#newKey').textContent();
 const result=await page.request.get(base+'/v1/models',{headers:{Authorization:'Bearer '+key}});if(result.status()!==200)throw Error('user key failed');await result.body();await page.locator('#dismiss').click();
 await page.locator('#profileName').fill('Browser QA renamed');await page.locator('#profileForm button').click();await page.waitForFunction(()=>document.getElementById('message').textContent==='Profile saved.');
 await page.setViewportSize({width:390,height:844});await page.screenshot({path:'/tmp/model-meter-mobile.png',fullPage:true});
 if(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth))throw Error('mobile page overflow');
 page.on('dialog',d=>d.accept());await page.locator('#keyRows button.danger').click();await page.waitForFunction(()=>document.querySelector('#keyRows').textContent.includes('Revoked'));
 const revoked=await page.request.get(base+'/v1/models',{headers:{Authorization:'Bearer '+key}});if(revoked.status()!==401)throw Error('revoked key accepted');
 await page.locator('#logout').click();await page.locator('#adminToken').fill(admin);await page.locator('#loginForm button').click();await page.locator('#dashboard').waitFor({state:'visible'});await page.locator('#userRows tr').filter({hasText:'Browser QA renamed'}).getByRole('button',{name:'Disable',exact:true}).click();await page.waitForFunction(()=>document.querySelector('#userRows').textContent.includes('Disabled'));
 if(errors.length)throw Error(errors.join('; '));console.log('Browser QA passed: administrator and user sign-in, user/key creation, isolation, profile update, charts, mobile layout, key revocation, user disable.');
 }finally{await browser.close()}
})().catch(e=>{console.error(e);process.exit(1)});
