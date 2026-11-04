// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
for(const [w,h,s,tag] of [[1100,760,1,'desktop'],[390,820,2,'phone']]){
 const c=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:s});
 for(const v of ['A','A-open','B','C','C-menu']){
  const p=await c.newPage();await p.goto(`file://${__dirname}/prototype.html?shot#${v}`);await p.evaluate(()=>document.fonts.ready);await p.waitForTimeout(800);
  await p.screenshot({path:`variant-${v}-${tag}.png`});await p.close();}
}await b.close()})();
