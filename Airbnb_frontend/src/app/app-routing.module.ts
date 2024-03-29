import { NgModule } from '@angular/core';
import { Routes, RouterModule } from '@angular/router';
import { LoginComponent } from './components/login/login.component';
import { RegisterComponent } from './components/register/register.component';
import { HomeComponent } from './components/home/home.component';
import { AccommodationComponent } from './components/accommodation/accommodation.component';
import { ProfileComponent } from './components/profile/profile.component';
import { EditProfileComponent } from './components/edit-profile/edit-profile.component';
import { CreateAccommodationComponent } from './components/create-accommodation/create-accommodation.component';
import { AuthGuard } from './services/auth.guard';
import { EmailVerificationComponent } from './components/email-verification/email-verification.component';
import { ForgotPasswordComponent } from './components/forgot-password/forgot-password.component';
import { ResetPasswordComponent } from './components/reset-password/reset-password.component';
import { ReservationsComponent } from './components/reservations/reservations.component';
import { DeleteAccountComponent } from './components/delete-account/delete-account.component';
import { RatingsComponent } from './components/ratings/ratings.component';
import { CreateAvailabilityComponent } from './components/create-availability/create-availability.component';
import { ReportComponent } from './components/report/report.component';

const routes: Routes = [
  {
      path: 'login',
      component: LoginComponent
    },
  {
    path: 'register',
    component: RegisterComponent
  },
  {
    path: 'home',
    component: HomeComponent
  },
  {
    path: 'accommodation',
    component: AccommodationComponent
  },
  { path: 'accommodation/:id',
  component: AccommodationComponent  },
  {
    path: 'profile',
    component: ProfileComponent,
    canActivate: [AuthGuard] 
  },
  {
    path: 'edit-profile',
    component: EditProfileComponent,
    canActivate: [AuthGuard] 
  },
  {
    path: 'create-accommodation',
    component: CreateAccommodationComponent,
    canActivate: [AuthGuard] ,
    data: {
      roles: ['Host']
    }
  },
  {
    path: 'email-verification',
    component: EmailVerificationComponent ,
  },
  {
    path: 'forgot-password',
    component: ForgotPasswordComponent ,
  },
  {
    path: 'reset-password',
    component: ResetPasswordComponent ,
  },

    {
    path: 'delete-account',
    component: DeleteAccountComponent ,
  },
  {
    path: 'reservations',
    component: ReservationsComponent,
    canActivate: [AuthGuard] ,
    data: {
      roles: ['Guest']
    }
  },
  {
    path: 'reviews',
    component: RatingsComponent
  },
   {
    path: 'reports',
    component: ReportComponent
  },
   {
    path: 'reports/:id',
    component: ReportComponent,
    canActivate: [AuthGuard] ,
    data: {
      roles: ['Host']
    }
  },
  // {
  //   path: 'availability-create/:accId',
  //   component: CreateAvailabilityComponent,
  //   canActivate: [AuthGuard],
  //   data: {
  //     roles: ['Host']
  //   }
  // },
];

@NgModule({
  imports: [RouterModule.forRoot(routes)],
  exports: [RouterModule]
})
export class AppRoutingModule { }
