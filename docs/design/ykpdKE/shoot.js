// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{
 const b=await chromium.launch();
 for(const [w,h,scale,tag] of [[1100,900,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:scale});
  for(const v of ['A','B','C']){
   const shots={
    live:p=>p.evaluate(()=>{demo.open(48)}),
    paused:async p=>{await p.evaluate(()=>{demo.more(1);demo.open(44)});await p.waitForTimeout(150);await p.evaluate(()=>demo.up());await p.waitForTimeout(150);await p.evaluate(()=>demo.more(2))},
    thinking:p=>p.evaluate(()=>{demo.thinking();demo.open(48)}),
    done:p=>p.evaluate(()=>{demo.open(51)}),
    none:p=>0,
   };
   for(const [st,fn] of Object.entries(shots)){
    const p=await ctx.newPage();
    // font loading is occasionally flaky in headless sandboxes: reload until both faces load
    for(let i=0;i<10;i++){
     await p.goto(`file://${__dirname}/prototype.html?shot#${v}${st==='done'?'-done':st==='none'?'-none':''}`);
     const ok=await p.evaluate(async()=>{try{await document.fonts.load('400 16px "Red Hat Display"');await document.fonts.load('14px "IBM Plex Mono"');return true}catch(e){return false}});
     if(ok)break;
    }
    await p.waitForTimeout(200);
    await fn(p);await p.waitForTimeout(250);
    await p.locator('#card').screenshot({path:`variant-${v}-${st}-${tag}.png`});
    await p.close();
   }
  }
 }
 await b.close();
})();
